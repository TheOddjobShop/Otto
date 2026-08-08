//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"runtime"
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
		// Acquire the otto slot before clearing, mirroring the rotator's
		// tryAcquire → Clear → resetInputTokens → release pattern. Without
		// the slot, a concurrent Otto turn that has already read a session ID
		// from the subprocess would restore it in runAndReply's Set call,
		// silently undoing the clear. The slot serialises the two operations
		// so /new has no lasting effect only when it truly wins the race.
		if !h.otto.tryAcquire("(/new)") {
			return commandResult{reply: "⏳ Otto is mid-turn — send /new again once he finishes to clear the session.", handled: true}
		}
		err := h.session.Clear()
		h.otto.resetInputTokens()
		h.otto.release()
		if err != nil {
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
	case "/tokens":
		if h.store == nil {
			return commandResult{reply: "📊 Token tracking is disabled (no store configured).", handled: true}
		}
		totals, err := h.store.UsageTotals(ctx)
		if err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ token totals failed: %v", err), handled: true}
		}
		bySrc, err := h.store.UsageBySource(ctx)
		if err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ token breakdown failed: %v", err), handled: true}
		}
		byModel, err := h.store.UsageByModel(ctx)
		if err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ token cost breakdown failed: %v", err), handled: true}
		}
		return commandResult{reply: formatUsage(totals, bySrc, byModel), handled: true}
	case "/restart":
		// Force-cancel an in-flight Otto call. Used when Otto seems wedged,
		// or when a long task isn't worth waiting for. The watchdog uses
		// the same suppress-error → cancel sequence at 10 minutes; this
		// just exposes the same lever to the user on demand.
		h.otto.mu.Lock()
		busy := h.otto.busy
		inflight := h.otto.currentPrompt
		// Capture the turn generation with the busy snapshot so the cancel
		// below can only hit the turn we observed — not a newer turn that
		// grabs the slot after we unlock.
		gen := h.otto.gen
		h.otto.mu.Unlock()
		if !busy {
			return commandResult{reply: "🔄 Otto isn't busy. Nothing to interrupt.", handled: true}
		}
		if !h.otto.cancelInflight(gen) {
			return commandResult{reply: "🔄 Otto just finished that task — nothing to interrupt.", handled: true}
		}
		// truncate() is rune-aware (handler.go) — safe for multi-byte
		// characters in the user's original prompt (e.g. emoji, CJK).
		preview := truncate(inflight, 80)
		return commandResult{
			reply:   fmt.Sprintf("🛑 Interrupted Otto. He was on: %q\nSession is preserved — re-send if you want him to resume.", preview),
			handled: true,
		}
	case "/version":
		return commandResult{
			reply:   fmt.Sprintf("version=%s os=%s/%s", version, runtime.GOOS, runtime.GOARCH),
			handled: true,
		}
	case "/update":
		return h.handleUpdateCommand()
	case "/status":
		return commandResult{reply: h.statusReport(ctx), handled: true}
	}
	return commandResult{}
}

// statusReport renders /status.
//
// Otto runs five things on timers — the model router, the session rotator, the
// bus drain, the watchdog and the store pruner — and until now none of them was
// observable without reading the journal. The original spec ruled out a web
// dashboard as a non-goal; a text command is not that, and the machinery being
// legible is what makes "he forgot" or "Toto never passed it on" diagnosable
// from the phone instead of from a terminal.
//
// Every lookup here is cheap and non-blocking: in-memory state plus one indexed
// COUNT. A status command must never be the slow thing, least of all when
// something is already wrong.
func (h *handler) statusReport(ctx context.Context) string {
	sid := h.session.ID()
	sessionEmpty := sid == ""
	if sessionEmpty {
		sid = "(none yet)"
	}

	h.otto.mu.Lock()
	busy := h.otto.busy
	inflight := h.otto.currentPrompt
	lastEvent := h.otto.lastEvent
	lastModel := h.otto.lastModel
	tokens := h.otto.lastInputTokens
	var idle time.Duration
	if !h.otto.lastUserMsg.IsZero() {
		idle = time.Since(h.otto.lastUserMsg)
	}
	h.otto.mu.Unlock()

	state := "idle"
	if busy {
		// truncate() is rune-aware (handler.go) — safe for multi-byte
		// characters sent by the user (e.g. emoji, CJK, Arabic).
		preview := truncate(inflight, 60)
		silence := time.Since(lastEvent).Round(time.Second)
		state = fmt.Sprintf("BUSY (silence=%s) on: %q", silence, preview)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "uptime=%s\n", time.Since(h.startedAt).Round(time.Second))
	fmt.Fprintf(&b, "state=%s\n", state)
	fmt.Fprintf(&b, "model=%s\n", modelLabel(lastModel))
	fmt.Fprintf(&b, "session=%s\n", sid)
	fmt.Fprintf(&b, "rotation=%s\n", describeRotation(tokens, idle, sessionEmpty, h.rotate))
	fmt.Fprintf(&b, "embeddings=%s\n", embedStatus.describe())
	fmt.Fprintf(&b, "prune=%s\n", pruneStatus.describe())

	if h.store != nil {
		if queued, ready, err := h.store.InboxDepth(ctx); err != nil {
			// Report the failure rather than omitting the line: a silently
			// missing row reads as "nothing queued", which is the opposite of
			// what an unreadable inbox means.
			fmt.Fprintf(&b, "bus=unavailable (%s)", truncate(err.Error(), 60))
		} else if queued == 0 {
			b.WriteString("bus=empty")
		} else {
			fmt.Fprintf(&b, "bus=%d queued, %d ready now", queued, ready)
		}
	} else {
		b.WriteString("bus=disabled (no store)")
	}
	return b.String()
}

// handleUpdateCommand returns the synchronous reply for /update and,
// when an install is actually available, kicks off the install +
// shutdown sequence in a goroutine. The goroutine outlives this call.
func (h *handler) handleUpdateCommand() commandResult {
	if h.updater == nil {
		return commandResult{reply: "Updater not initialized.", handled: true}
	}
	p := h.updater.Pending()
	if p == nil {
		return commandResult{
			reply:   fmt.Sprintf("No update available. You're on %s.", h.updater.currentVersion),
			handled: true,
		}
	}

	h.otto.mu.Lock()
	busy := h.otto.busy
	inflight := h.otto.currentPrompt
	h.otto.mu.Unlock()

	reply := fmt.Sprintf(
		"Starting update to %s for %s/%s…",
		p.Tag, runtime.GOOS, runtime.GOARCH,
	)
	if busy {
		// truncate() is rune-aware (handler.go) — safe for multi-byte
		// characters in the inflight prompt.
		preview := truncate(inflight, 60)
		reply += fmt.Sprintf(" (Otto is mid-task on %q — that work will be interrupted.)", preview)
	}

	go h.runUpdate()
	return commandResult{reply: reply, handled: true}
}

// runUpdate is the side-effect goroutine spawned by /update. Reports
// failures back to the user; on success, sends a confirmation and
// exits the process.
func (h *handler) runUpdate() {
	ctx := context.Background()
	chatID := h.updater.chatID
	if err := h.updater.Install(ctx); err != nil {
		msg := fmt.Sprintf("⚠️ Update failed: %v", err)
		if sendErr := telegram.SendChunked(ctx, h.bot, chatID, msg); sendErr != nil {
			log.Printf("update: send failure msg: %v", sendErr)
		}
		return
	}
	h.updater.Exit()
}
