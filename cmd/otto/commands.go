//go:build unix

package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"otto/internal/telegram"
)

type commandResult struct {
	reply   string
	handled bool
}

func (h *handler) tryCommand(ctx context.Context, u telegram.Update) commandResult {
	text := strings.TrimSpace(u.Text)
	if !strings.HasPrefix(text, "/") {
		return commandResult{}
	}
	parts := strings.Fields(text)
	switch parts[0] {
	case "/new":
		if err := h.session.Clear(); err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ clear failed: %v", err), handled: true}
		}
		return commandResult{reply: "✨ Started new session — your next message will start a fresh conversation.", handled: true}
	case "/whoami":
		sid := h.session.ID()
		if sid == "" {
			sid = "(none yet — send a message to start)"
		}
		return commandResult{
			reply:   fmt.Sprintf("user=%d session=%s", u.UserID, sid),
			handled: true,
		}
	case "/restart":
		// In-flight Claude calls hold h.mu; tryCommand runs under h.mu, so by
		// the time we get here any in-flight call has already returned. The
		// command exists for symmetry with the design and as a no-op ack.
		return commandResult{reply: "🔄 No in-flight call. (Send /new to start fresh.)", handled: true}
	case "/status":
		sid := h.session.ID()
		if sid == "" {
			sid = "(none yet)"
		}
		return commandResult{
			reply:   fmt.Sprintf("uptime=%s session=%s", time.Since(h.startedAt).Round(time.Second), sid),
			handled: true,
		}
	}
	return commandResult{}
}
