//go:build unix

package main

import (
	"testing"
	"time"
)

func testRotateConfig() rotateConfig {
	return rotateConfig{
		ctxTokens:  200000,
		hard:       0.85,
		idleWindow: 15 * time.Minute,
	}
}

func TestShouldRotateIdleResetsRegardlessOfSize(t *testing.T) {
	c := testRotateConfig()
	// A small session that's been idle past the window still rotates — the
	// periodic "reset on inactivity" behaviour, independent of token count.
	if !shouldRotate(5000, 16*time.Minute, c) {
		t.Error("idle past window should rotate even for a small session")
	}
	if !shouldRotate(120000, 20*time.Minute, c) {
		t.Error("idle past window should rotate")
	}
}

func TestShouldRotateActiveBelowHardDoesNotRotate(t *testing.T) {
	c := testRotateConfig()
	// Recently active (not idle) and below the hard cap: keep the session.
	if shouldRotate(5000, 1*time.Minute, c) {
		t.Error("small, recently-active session should not rotate")
	}
	if shouldRotate(120000, 1*time.Minute, c) {
		t.Error("below hard cap and not idle should not rotate")
	}
}

func TestShouldRotateHardIgnoresIdle(t *testing.T) {
	c := testRotateConfig()
	if !shouldRotate(180000, 10*time.Second, c) {
		t.Error("hard threshold should rotate regardless of idle")
	}
}

func TestShouldRotateZeroTokens(t *testing.T) {
	c := testRotateConfig()
	if shouldRotate(0, time.Hour, c) {
		t.Error("zero tokens should not rotate")
	}
}

func TestShouldRotateZeroCtxIsSafe(t *testing.T) {
	c := testRotateConfig()
	c.ctxTokens = 0
	if shouldRotate(100000, time.Hour, c) {
		t.Error("zero ctxTokens must not divide-by-zero into a rotation")
	}
}
