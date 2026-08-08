//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"time"
)

// rotateCheckInterval is how often the rotator evaluates whether to rotate.
const rotateCheckInterval = 1 * time.Minute

// hardRotateActiveGrace is how long the user must have been silent before the
// hard cap will clear an over-budget session. It stops the cap from wiping a
// session mid-conversation — e.g. right after a data-heavy fetch that balloons
// context — which would make Otto "forget" between two back-to-back messages.
// The idle reset (idleWindow) still clears regardless of size once the user
// has been quiet that much longer.
const hardRotateActiveGrace = 5 * time.Minute

// petRotator is a pet (Toto/Toot) whose conversational session is cleared
// after a period of inactivity, mirroring Otto's idle reset. Without this a
// pet session lives forever and can answer from stale history.
type petRotator interface {
	rotateIfIdle(window time.Duration)
}

// rotateConfig holds the rotation thresholds, resolved from config at startup.
type rotateConfig struct {
	ctxTokens  int
	hard       float64
	idleWindow time.Duration
	// flush enables the pre-clear memory distillation pass (see flush.go).
	flush bool
}

// shouldRotate decides whether the current session should be cleared. tokens is
// the latest observed session input-token count; idle is how long since the
// last user message. Returns false for a zero/invalid context size (no
// divide-by-zero) and for a session with no observed tokens.
func shouldRotate(tokens int, idle time.Duration, c rotateConfig) bool {
	if c.ctxTokens <= 0 || tokens <= 0 {
		return false
	}
	// Idle reset: once the user has been quiet for the idle window, clear the
	// session regardless of size so the next message starts fresh. Durable
	// facts live in the always-injected memory core (USER.md + MEMORY.md), so
	// nothing important is lost — this just bounds per-message context growth
	// and cost. This is the "reset every ~15 minutes of inactivity" behaviour.
	if idle >= c.idleWindow {
		return true
	}
	// Hard cap: an over-budget session rotates once it grows past this fraction
	// of context — but only after the user has paused for the active grace, so
	// the cap never wipes context mid-conversation. A single heavy turn (e.g. a
	// full Notion backlog dump) can push past the cap, and the user's very next
	// message must still see that context. Once they pause, the bloated session
	// clears so the following turn starts cheap.
	if float64(tokens)/float64(c.ctxTokens) >= c.hard && idle >= hardRotateActiveGrace {
		return true
	}
	return false
}

// describeRotation renders the rotator's state for /status: how full the
// session is and how long until it clears.
//
// Rotation is the single most confusing thing Otto does from the outside — the
// user experiences it as "he forgot", with no visible cause — so the countdown
// is worth surfacing plainly. sessionEmpty distinguishes "already cleared" from
// "tracking nothing yet", which look the same in the token count but mean
// opposite things.
func describeRotation(tokens int, idle time.Duration, sessionEmpty bool, c rotateConfig) string {
	if c.ctxTokens <= 0 {
		return "disabled"
	}
	if sessionEmpty {
		return "no active session — next message starts fresh"
	}
	if tokens <= 0 {
		return "no turn observed yet this session"
	}
	pct := float64(tokens) / float64(c.ctxTokens) * 100
	usage := fmt.Sprintf("%d tok (%.0f%% of %d)", tokens, pct, c.ctxTokens)

	if shouldRotate(tokens, idle, c) {
		// Due, but the rotator still has to win the Otto slot, so say why it
		// might not have happened yet rather than implying it has.
		return usage + " — due now, clears at the next tick Otto is free"
	}

	// Both paths can be pending at once; report whichever fires first.
	untilIdle := c.idleWindow - idle
	best := untilIdle
	reason := "idle reset"
	if float64(tokens)/float64(c.ctxTokens) >= c.hard {
		if untilGrace := hardRotateActiveGrace - idle; untilGrace < best {
			best, reason = untilGrace, "hard cap (over budget, waiting for a pause)"
		}
	}
	if best < 0 {
		best = 0
	}
	return fmt.Sprintf("%s — %s in %s", usage, reason, best.Round(time.Second))
}

// runRotator is a long-lived goroutine (started from main) that periodically
// clears Otto's session once it has grown past a threshold and the user is
// idle, bounding per-turn token cost. It claims the Otto slot before clearing
// so it can never race a live turn; if Otto is busy it waits for the next
// tick. Exits when ctx is cancelled.
func (h *handler) runRotator(ctx context.Context) {
	if h.rotate.ctxTokens <= 0 {
		log.Printf("rotator: disabled (ctxTokens<=0)")
		return
	}
	ticker := time.NewTicker(rotateCheckInterval)
	defer ticker.Stop()
	for {
		h.maybeRotate(ctx)
		// Pets clear their own sessions on the same idle window so they don't
		// answer from stale history (e.g. an old version number).
		for _, p := range h.petRotators {
			p.rotateIfIdle(h.rotate.idleWindow)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// maybeRotate performs one rotation evaluation: if the session is non-empty,
// over threshold, and Otto is free, optionally distill it into the memory core
// and then clear it.
//
// ctx bounds the flush pass; rotation itself is not cancellable (a clear that
// has been decided on should complete).
func (h *handler) maybeRotate(ctx context.Context) {
	sessionID := h.session.ID()
	if sessionID == "" {
		return
	}
	tokens, idle := h.otto.rotationSnapshot()
	if !shouldRotate(tokens, idle, h.rotate) {
		return
	}
	if !h.otto.tryAcquire("(session rotation)") {
		return // Otto busy; retry next tick
	}
	// The slot is held across the flush as well as the clear. The flush
	// resumes this very session, so a concurrent Otto turn would interleave
	// with it; and clearing between the two would distill a session that no
	// longer exists.
	if shouldFlush(h.rotate.flush, h.mem != nil, tokens) {
		h.runFlush(ctx, sessionID, tokens)
	}
	err := h.session.Clear()
	h.otto.resetInputTokens()
	h.otto.release()
	if err != nil {
		log.Printf("rotator: clear session: %v", err)
		return
	}
	log.Printf("rotator: rotated session (tokens=%d idle=%s) — next message starts fresh", tokens, idle.Round(time.Second))
}
