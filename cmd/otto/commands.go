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
		// Force-cancel an in-flight Otto call. Used when Otto seems wedged,
		// or when a long task isn't worth waiting for. The watchdog uses
		// the same suppress-error → cancel sequence at 10 minutes; this
		// just exposes the same lever to the user on demand.
		h.otto.mu.Lock()
		busy := h.otto.busy
		cancel := h.otto.cancel
		inflight := h.otto.currentPrompt
		h.otto.mu.Unlock()
		if !busy {
			return commandResult{reply: "🔄 Otto isn't busy. Nothing to interrupt.", handled: true}
		}
		h.otto.markSuppressError()
		if cancel != nil {
			cancel()
		}
		preview := inflight
		if len(preview) > 80 {
			preview = preview[:80] + "…"
		}
		return commandResult{
			reply:   fmt.Sprintf("🛑 Interrupted Otto. He was on: %q\nSession is preserved — re-send if you want him to resume.", preview),
			handled: true,
		}
	case "/status":
		sid := h.session.ID()
		if sid == "" {
			sid = "(none yet)"
		}
		h.otto.mu.Lock()
		busy := h.otto.busy
		inflight := h.otto.currentPrompt
		lastEvent := h.otto.lastEvent
		h.otto.mu.Unlock()

		state := "idle"
		if busy {
			preview := inflight
			if len(preview) > 60 {
				preview = preview[:60] + "…"
			}
			silence := time.Since(lastEvent).Round(time.Second)
			state = fmt.Sprintf("BUSY (silence=%s) on: %q", silence, preview)
		}
		return commandResult{
			reply: fmt.Sprintf("uptime=%s\nstate=%s\nsession=%s",
				time.Since(h.startedAt).Round(time.Second), state, sid),
			handled: true,
		}
	}
	return commandResult{}
}
