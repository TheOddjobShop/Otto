//go:build unix

package main

import (
	"bufio"
	"context"
	"fmt"
	"html"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"otto/internal/telegram"
)

// ttyBot is a telegram.BotClient that reads user input from stdin and
// writes bot replies to stdout. Used by `otto -tty` for local end-to-end
// testing without needing a real Telegram account or bot token.
//
// All routing, Otto-vs-Toto handoff, watchdog timers, MCP/Claude calls,
// and persistence still go through their real implementations — only the
// Telegram I/O is replaced. This means a `-tty` session pollutes the
// real Otto + Toto session-id files, so use a throwaway config or `/new`
// before testing things that matter.
type ttyBot struct {
	userID   int64
	updateID atomic.Int64

	lines    chan string
	shutdown context.CancelFunc

	once sync.Once
}

// newTTYBot starts a goroutine that reads stdin line-by-line. EOF on stdin
// (Ctrl-D) calls shutdown so the polling loop exits cleanly.
func newTTYBot(userID int64, shutdown context.CancelFunc) *ttyBot {
	t := &ttyBot{
		userID:   userID,
		lines:    make(chan string),
		shutdown: shutdown,
	}
	go t.readStdin()
	return t
}

func (t *ttyBot) readStdin() {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // allow long pasted prompts
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		t.lines <- line
	}
	fmt.Fprintln(os.Stderr, "\n[tty] stdin closed — shutting down")
	t.once.Do(t.shutdown)
}

func (t *ttyBot) GetUpdates(ctx context.Context, offset int) ([]telegram.Update, error) {
	select {
	case line := <-t.lines:
		id := int(t.updateID.Add(1))
		return []telegram.Update{{
			UpdateID: id,
			ChatID:   t.userID,
			UserID:   t.userID,
			Text:     line,
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SendMessage prints Otto's reply. Otto uses plain SendMessage and Toto
// uses SendMessageHTML, so this prefix doubles as a "who's talking" label.
func (t *ttyBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	fmt.Printf("\n\033[1;36motto:\033[0m %s\n\n", text)
	return nil
}

// SendMessageHTML prints Toto's reply. Strips the only tag Toto emits
// (<pre>) and unescapes HTML entities so the ASCII art renders cleanly
// in a monospace terminal.
func (t *ttyBot) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	plain := strings.ReplaceAll(text, "<pre>", "")
	plain = strings.ReplaceAll(plain, "</pre>", "")
	plain = html.UnescapeString(plain)
	fmt.Printf("\n\033[1;33mtoto:\033[0m\n%s\n\n", plain)
	return nil
}

func (t *ttyBot) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return nil, "", fmt.Errorf("tty mode: photo download not supported")
}

// Compile-time check that ttyBot satisfies the BotClient interface.
var _ telegram.BotClient = (*ttyBot)(nil)
