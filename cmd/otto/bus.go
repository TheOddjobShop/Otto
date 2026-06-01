//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"time"
	"unicode/utf8"

	"otto/internal/store"
	"otto/internal/telegram"
)

// busDrainInterval is how often runBusDrain polls the inbox table. Kept
// short so an Otto→Toto ping feels conversational (sub-second), but long
// enough to coalesce bursts and keep idle CPU at ~zero. Package var so
// tests can crank it down.
var busDrainInterval = 250 * time.Millisecond

// busBannerPreviewRunes caps the inline body preview in the banner that
// precedes each dispatched bus message. 80 runes is enough for a tweet-
// sized hint without blowing up a Telegram bubble.
const busBannerPreviewRunes = 80

// runBusDrain polls the inbox table on busDrainInterval and dispatches
// each row to the addressed agent. Returns when ctx is cancelled. Bus
// messages are explicitly user-visible per design: every dispatch is
// preceded by a plain Telegram banner so the user can see Toto's
// forwards and Otto's pings happen.
//
// Loop guard: messages with source=="agent" are dispatched under a
// context tagged via store.WithAgentHop, so any downstream tool handler
// that tries to Enqueue another bus message will fail with
// store.ErrBusLoopGuard rather than start a ping-pong.
func (h *handler) runBusDrain(ctx context.Context) {
	if h.store == nil {
		return
	}
	t := time.NewTicker(busDrainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		msgs, err := h.store.DequeueAll(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("bus: dequeue error: %v", err)
			continue
		}
		for _, m := range msgs {
			h.dispatchBusMessage(ctx, m)
		}
	}
}

// dispatchBusMessage routes one bus message to the addressed agent.
// The banner is sent first so the user always knows a bus event
// happened ("Show all" — bus traffic is explicit, not silent).
//
// For agent-sourced messages, the dispatch context is wrapped via
// store.WithAgentHop so a tool handler running inside Otto/Toto/Toot
// can't enqueue another agent-to-agent message and start a loop.
func (h *handler) dispatchBusMessage(ctx context.Context, m store.InboxMsg) {
	dispatchCtx := ctx
	if m.Source == "agent" {
		dispatchCtx = store.WithAgentHop(ctx)
	}

	chatID := h.allow.UserID()
	if chatID == 0 {
		log.Printf("bus: no chat id available, dropping message id=%d", m.ID)
		return
	}

	banner := busBanner(m)
	if err := telegram.SendChunked(ctx, h.bot, chatID, banner); err != nil {
		log.Printf("bus: banner send error: %v", err)
		// Continue anyway — the actual routed reply is still worth attempting.
	}

	switch m.Target {
	case "otto":
		u := telegram.Update{
			UserID: chatID,
			ChatID: chatID,
			Text:   m.Body,
		}
		acquired, snap := h.otto.tryAcquireOrSnapshot(u.Text)
		if acquired {
			defer h.otto.release()
			h.handleMessage(dispatchCtx, u)
			return
		}
		// Otto busy — fall back to Toto, matching the live-dispatch path.
		log.Printf("bus: otto busy on forwarded msg id=%d (silence=%s)", m.ID, snap.Silence.Round(time.Second))
		if h.toto != nil {
			h.toto.BusyReply(dispatchCtx, chatID, u.Text, snap.CurrentPrompt, snap.Snippet)
		}
	case "toto":
		if h.toto == nil {
			log.Printf("bus: no toto wired, dropping id=%d", m.ID)
			return
		}
		h.toto.Reply(dispatchCtx, chatID, m.Body)
	case "toot":
		toot := h.findToot()
		if toot == nil {
			log.Printf("bus: no toot wired, dropping id=%d", m.ID)
			return
		}
		toot.Reply(dispatchCtx, chatID, m.Body)
	default:
		log.Printf("bus: unknown target %q (id=%d)", m.Target, m.ID)
	}
}

// findToot pulls the Toot pet out of the registry. Toot isn't held as
// its own field on handler (only Toto is, because the dispatch fast-path
// uses BusyReply); for the bus we look it up by name.
func (h *handler) findToot() *Toot {
	if h.pets == nil {
		return nil
	}
	for _, p := range h.pets.pets {
		if t, ok := p.(*Toot); ok {
			return t
		}
	}
	return nil
}

// busBanner builds the user-visible "↪️ X → Y: preview" line that
// precedes every dispatched bus message. Source/sender labels are
// chosen so the user sees an honest who-from-and-to.
func busBanner(m store.InboxMsg) string {
	preview := truncateRunes(m.Body, busBannerPreviewRunes)
	from := m.Sender
	if from == "" {
		from = m.Source // "user" if sender unset
	}
	if m.Target == "otto" {
		return fmt.Sprintf("↪️ forwarded from %s: %s", from, preview)
	}
	return fmt.Sprintf("↪️ %s → %s: %s", from, m.Target, preview)
}

// truncateRunes returns s if it has ≤ n runes, else its n-rune prefix
// plus an ellipsis. Rune-aware so a multibyte tail doesn't get sliced
// mid-codepoint.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	out := make([]rune, 0, n+1)
	count := 0
	for _, r := range s {
		if count == n {
			break
		}
		out = append(out, r)
		count++
	}
	out = append(out, '…')
	return string(out)
}
