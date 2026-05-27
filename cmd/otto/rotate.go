//go:build unix

package main

import "time"

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
	if frac >= c.hard {
		return true
	}
	if frac >= c.soft && idle >= c.idleWindow {
		return true
	}
	return false
}
