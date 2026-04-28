package telegram

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var downloadHTTPClient = &http.Client{Timeout: 30 * time.Second}

// Update is the slice of a Telegram update Otto cares about. It carries
// either a regular message (Text/PhotoIDs) or a button-tap (CallbackQueryID
// non-empty) — never both.
type Update struct {
	UpdateID int
	ChatID   int64
	UserID   int64
	Text     string
	PhotoIDs []string // largest-size photo file_id per photo, if any

	// Set when this update is a tap on an inline-keyboard button.
	CallbackQueryID   string
	CallbackData      string
	CallbackMessageID int // ID of the message that owned the keyboard
}

// IsCallback reports whether this update is an inline-keyboard button tap
// rather than a normal message.
func (u Update) IsCallback() bool { return u.CallbackQueryID != "" }

// InlineButton is one cell in an inline keyboard. CallbackData is sent back
// when the button is tapped (max 64 bytes per the Telegram Bot API).
type InlineButton struct {
	Text         string
	CallbackData string
}

// BotClient is the surface of Telegram operations Otto needs. Defined as an
// interface so cmd/otto can be unit-tested with a fake.
//
// Context propagation: tgbotapi/v5 does not pass context into its HTTP layer,
// so the ctx parameter on GetUpdates and SendMessage is advisory only — those
// calls will not be cancelled when ctx is. DownloadFile honors ctx because it
// uses our own http.NewRequestWithContext.
type BotClient interface {
	GetUpdates(ctx context.Context, offset int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	// SendMessageHTML sends `text` with parse_mode=HTML so tags like
	// <pre>...</pre> render as monospace. Caller is responsible for
	// escaping any literal <, >, & in the body via html.EscapeString.
	SendMessageHTML(ctx context.Context, chatID int64, text string) error
	SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]InlineButton) error
	AnswerCallbackQuery(ctx context.Context, queryID, text string) error
	DownloadFile(ctx context.Context, fileID string) ([]byte, string, error)
}

type realClient struct {
	api *tgbotapi.BotAPI
}

// NewBotClient returns a real Telegram client. apiURLTemplate is the format
// string used by tgbotapi (e.g. "https://api.telegram.org/bot%s/%s"); pass
// httptest.NewServer URL + "/bot%s/%s" in tests.
func NewBotClient(token, apiURLTemplate string) (BotClient, error) {
	api, err := tgbotapi.NewBotAPIWithAPIEndpoint(token, apiURLTemplate)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}
	api.Debug = false
	return &realClient{api: api}, nil
}

func (c *realClient) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	cfg := tgbotapi.NewUpdate(offset)
	cfg.Timeout = 30
	updates, err := c.api.GetUpdates(cfg)
	if err != nil {
		return nil, fmt.Errorf("telegram: get updates: %w", err)
	}
	out := make([]Update, 0, len(updates))
	for _, u := range updates {
		out = append(out, fromTGUpdate(u))
	}
	return out, nil
}

func (c *realClient) SendMessage(ctx context.Context, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := c.api.Send(msg); err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	return nil
}

func (c *realClient) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	if _, err := c.api.Send(msg); err != nil {
		return fmt.Errorf("telegram: send-html: %w", err)
	}
	return nil
}

func (c *realClient) SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]InlineButton) error {
	msg := tgbotapi.NewMessage(chatID, text)
	rows := make([][]tgbotapi.InlineKeyboardButton, 0, len(buttons))
	for _, row := range buttons {
		cells := make([]tgbotapi.InlineKeyboardButton, 0, len(row))
		for _, b := range row {
			cells = append(cells, tgbotapi.NewInlineKeyboardButtonData(b.Text, b.CallbackData))
		}
		rows = append(rows, cells)
	}
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	if _, err := c.api.Send(msg); err != nil {
		return fmt.Errorf("telegram: send-with-buttons: %w", err)
	}
	return nil
}

func (c *realClient) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	cb := tgbotapi.NewCallback(queryID, text)
	if _, err := c.api.Request(cb); err != nil {
		return fmt.Errorf("telegram: answer callback: %w", err)
	}
	return nil
}

func (c *realClient) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	url, err := c.api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, "", fmt.Errorf("telegram: get file url: %w", err)
	}
	return downloadURL(ctx, url)
}

func fromTGUpdate(u tgbotapi.Update) Update {
	out := Update{UpdateID: u.UpdateID}
	// Inline-keyboard taps come through as a CallbackQuery, not a Message.
	if u.CallbackQuery != nil {
		out.CallbackQueryID = u.CallbackQuery.ID
		out.CallbackData = u.CallbackQuery.Data
		if u.CallbackQuery.From != nil {
			out.UserID = u.CallbackQuery.From.ID
		}
		if u.CallbackQuery.Message != nil {
			out.ChatID = u.CallbackQuery.Message.Chat.ID
			out.CallbackMessageID = u.CallbackQuery.Message.MessageID
		}
		return out
	}
	if u.Message == nil {
		return out
	}
	out.ChatID = u.Message.Chat.ID
	if u.Message.From != nil {
		out.UserID = u.Message.From.ID
	}
	out.Text = u.Message.Text
	if u.Message.Caption != "" && out.Text == "" {
		out.Text = u.Message.Caption
	}
	if len(u.Message.Photo) > 0 {
		// Telegram returns multiple sizes; pick the largest.
		largest := u.Message.Photo[len(u.Message.Photo)-1]
		out.PhotoIDs = []string{largest.FileID}
	}
	return out
}

func downloadURL(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := downloadHTTPClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("telegram: download status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}
