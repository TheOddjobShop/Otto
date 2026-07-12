package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var downloadHTTPClient = &http.Client{Timeout: 30 * time.Second}

// maxPhotoBytes caps the in-memory photo download. Telegram's bot API
// caps photos at 10 MiB; the extra headroom protects against malformed
// responses or content-type spoofing that could otherwise exhaust memory.
const maxPhotoBytes = 25 * 1024 * 1024

// Update is the slice of a Telegram update Otto cares about. It carries a
// regular message (Text and/or PhotoIDs).
type Update struct {
	UpdateID int
	ChatID   int64
	UserID   int64
	Text     string
	PhotoIDs []string // largest-size photo file_id per photo, if any
}

// BotClient is the surface of Telegram operations Otto needs. Defined as an
// interface so cmd/otto can be unit-tested with a fake.
//
// Context propagation: tgbotapi/v5 does not pass context into its HTTP layer,
// so the ctx parameter on GetUpdates and SendMessage is advisory only — those
// calls will not be cancelled when ctx is. DownloadFile honors ctx only for
// the file-body download (which uses our own http.NewRequestWithContext); the
// initial GetFile lookup goes through tgbotapi and is bounded only by the
// client's 30s timeout.
type BotClient interface {
	GetUpdates(ctx context.Context, offset int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	// SendMessageHTML sends `text` with parse_mode=HTML so tags like
	// <pre>...</pre> render as monospace. Caller is responsible for
	// escaping any literal <, >, & in the body via html.EscapeString.
	SendMessageHTML(ctx context.Context, chatID int64, text string) error
	DownloadFile(ctx context.Context, fileID string) ([]byte, string, error)
}

type realClient struct {
	api   *tgbotapi.BotAPI
	token string
}

// redactToken replaces the bot token in an error's text with "<redacted>".
// tgbotapi builds every request URL as .../bot<TOKEN>/<method> and Go's
// http.Client wraps transport failures in *url.Error, whose Error() string
// includes the full URL — token and all. Left unredacted, a network blip
// would leak the token into the daemon log and even into Telegram chat
// replies (e.g. the photo-download failure notice).
func redactToken(err error, token string) error {
	if err == nil || token == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "<redacted>"))
}

// stripURLError unwraps a *url.Error to its underlying cause. The download
// CDN URL embeds the bot token (GetFileDirectURL), so the wrapper's
// URL-bearing message must never escape into logs or chat replies.
func stripURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// NewBotClient returns a real Telegram client. apiURLTemplate is the format
// string used by tgbotapi (e.g. "https://api.telegram.org/bot%s/%s"); pass
// httptest.NewServer URL + "/bot%s/%s" in tests.
func NewBotClient(token, apiURLTemplate string) (BotClient, error) {
	api, err := tgbotapi.NewBotAPIWithAPIEndpoint(token, apiURLTemplate)
	if err != nil {
		// NewBotAPIWithAPIEndpoint calls GetMe, so a network failure here
		// carries a token-bearing *url.Error.
		return nil, fmt.Errorf("telegram: %w", redactToken(err, token))
	}
	api.Debug = false
	// tgbotapi's default http.Client has no timeout and its requests carry
	// no context, so a hung Telegram connection would block the GetUpdates
	// goroutine (and its connection) forever. Bound it with a client-side
	// timeout comfortably larger than the 5s long-poll window.
	api.Client = &http.Client{Timeout: 30 * time.Second}
	return &realClient{api: api, token: token}, nil
}

func (c *realClient) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	cfg := tgbotapi.NewUpdate(offset)
	// Short long-poll so SIGTERM shutdown completes promptly. tgbotapi's
	// GetUpdates does not honor context.Context, so a longer timeout
	// blocks the polling goroutine until the server replies (empty or
	// not), delaying systemd/launchctl restart by the same window. 5s
	// keeps shutdown responsive without flooding the Telegram API.
	cfg.Timeout = 5
	// Filter to message updates only — Otto's handler ignores inline
	// queries, callback queries, channel posts, etc., so there's no
	// reason to pull them across the wire.
	cfg.AllowedUpdates = []string{"message"}

	// Run the blocking call in a goroutine so ctx cancellation can
	// unblock the polling loop's shutdown path even though tgbotapi
	// itself can't be cancelled. The orphaned goroutine drains and
	// discards its result when ctx is already done.
	type result struct {
		updates []tgbotapi.Update
		err     error
	}
	done := make(chan result, 1)
	go func() {
		updates, err := c.api.GetUpdates(cfg)
		done <- result{updates: updates, err: err}
	}()
	var r result
	select {
	case r = <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, fmt.Errorf("telegram: get updates: %w", redactToken(r.err, c.token))
	}
	out := make([]Update, 0, len(r.updates))
	for _, u := range r.updates {
		out = append(out, fromTGUpdate(u))
	}
	return out, nil
}

func (c *realClient) SendMessage(ctx context.Context, chatID int64, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := c.api.Send(msg); err != nil {
		return fmt.Errorf("telegram: send: %w", redactToken(err, c.token))
	}
	return nil
}

func (c *realClient) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = tgbotapi.ModeHTML
	if _, err := c.api.Send(msg); err != nil {
		return fmt.Errorf("telegram: send-html: %w", redactToken(err, c.token))
	}
	return nil
}

func (c *realClient) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	url, err := c.api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, "", fmt.Errorf("telegram: get file url: %w", redactToken(err, c.token))
	}
	return downloadURL(ctx, url)
}

func fromTGUpdate(u tgbotapi.Update) Update {
	out := Update{UpdateID: u.UpdateID}
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
		return nil, "", stripURLError(err)
	}
	resp, err := downloadHTTPClient.Do(req)
	if err != nil {
		return nil, "", stripURLError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("telegram: download status %d", resp.StatusCode)
	}
	// Read one byte past the cap so we can distinguish "exactly at cap" from
	// "exceeds cap" and return an explicit error instead of silently
	// truncating into a corrupt image.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPhotoBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(body) > maxPhotoBytes {
		return nil, "", fmt.Errorf("telegram: download exceeds %d bytes", maxPhotoBytes)
	}
	return body, resp.Header.Get("Content-Type"), nil
}
