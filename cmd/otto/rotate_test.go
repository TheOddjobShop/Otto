//go:build unix

package main

import (
	"testing"
	"time"
)

func testRotateConfig() rotateConfig {
	return rotateConfig{
		ctxTokens:  200000,
		soft:       0.50,
		hard:       0.85,
		idleWindow: 15 * time.Minute,
	}
}

func TestShouldRotateBelowSoftNeverRotates(t *testing.T) {
	c := testRotateConfig()
	if shouldRotate(80000, time.Hour, c) {
		t.Error("below soft threshold should never rotate")
	}
}

func TestShouldRotateSoftWaitsForIdle(t *testing.T) {
	c := testRotateConfig()
	if shouldRotate(120000, 1*time.Minute, c) {
		t.Error("soft-eligible but not idle should not rotate")
	}
	if !shouldRotate(120000, 20*time.Minute, c) {
		t.Error("soft-eligible and idle should rotate")
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
