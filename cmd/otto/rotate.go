//go:build unix

package main

import (
	"context"
	"log"
	"time"
)

// rotateCheckInterval is how often the rotator evaluates whether to rotate.
const rotateCheckInterval = 1 * time.Minute

// rotateConfig holds the rotation thresholds, resolved from config at startup.
type rotateConfig struct {
	ctxTokens  int
	soft       float64
	hard       float64
	idleWindow time.Duration
}

// shouldRotate decides whether the current session should be cleared. tokens is
// the latest observed session input-token count; idle is how long since the
// last user message. Returns false for a zero/invalid context size (no
// divide-by-zero) and for a session with no observed tokens.
func shouldRotate(tokens int, idle time.Duration, c rotateConfig) bool {
	if c.ctxTokens <= 0 || tokens <= 0 {
		return false
	}
	frac := float64(tokens) / float64(c.ctxTokens)
	// Hard cap intentionally ignores idle: at this size we rotate at the next
	// free moment regardless of how recently the user spoke. Do not gate it on
	// idleWindow.
	if frac >= c.hard {
		return true
	}
	if frac >= c.soft && idle >= c.idleWindow {
		return true
	}
	return false
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
		h.maybeRotate()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// maybeRotate performs one rotation evaluation: if the session is non-empty,
// over threshold, and Otto is free, clear it.
func (h *handler) maybeRotate() {
	if h.session.ID() == "" {
		return
	}
	tokens, idle := h.otto.rotationSnapshot()
	if !shouldRotate(tokens, idle, h.rotate) {
		return
	}
	if !h.otto.tryAcquire("(session rotation)") {
		return // Otto busy; retry next tick
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
