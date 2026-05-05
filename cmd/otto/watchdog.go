//go:build unix

package main

import (
	"context"
	"log"
	"time"
)

const (
	// watchdogTick is how often the watchdog checks Otto's last-event time.
	watchdogTick = 30 * time.Second
	// watchdogWarnAfter is the silence threshold for sending a Toto-flavoured
	// "still alive?" message. Below this, Otto is considered to be working
	// normally, even on a long task.
	watchdogWarnAfter = 5 * time.Minute
	// watchdogCancelAfter is the silence threshold for forcibly cancelling
	// Otto's subprocess. At this point we assume he's wedged.
	watchdogCancelAfter = 10 * time.Minute
)

// runWatchdog observes ottoState.lastEvent while Otto is busy. It exits
// when done is closed (Otto finished naturally) or ctx is cancelled
// (process shutdown).
//
// At watchdogWarnAfter of silence: Toto sends a "he's been quiet for X
// minutes" message to the same chat that triggered the call.
//
// At watchdogCancelAfter of silence: ottoState.cancel is invoked (which
// SIGKILLs the claude subprocess via context cancellation in runner.go),
// suppressError is set so handleMessage doesn't surface the resulting
// "context canceled" as a Claude error, and Toto sends a reboot message.
//
// ctx is the polling-loop's parent context — the watchdog uses it for
// the Toto send so the reboot message survives the in-flight call's
// teardown. done is closed by the caller when Otto's call returns,
// signaling immediate watchdog exit (vs polling for !busy).
func (h *handler) runWatchdog(ctx context.Context, chatID int64, done <-chan struct{}) {
	ticker := time.NewTicker(watchdogTick)
	defer ticker.Stop()

	warned := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
			h.otto.mu.Lock()
			if !h.otto.busy {
				h.otto.mu.Unlock()
				return
			}
			silence := time.Since(h.otto.lastEvent)
			cancel := h.otto.cancel
			h.otto.mu.Unlock()

			if silence >= watchdogCancelAfter {
				log.Printf("watchdog: otto silent for %s — cancelling subprocess", silence.Round(time.Second))
				h.otto.markSuppressError()
				if cancel != nil {
					cancel()
				}
				h.toto.SystemMessage(ctx, chatID,
					"otto wedged. i rebooted him. try sending your original message again whenever — he'll pick up where he left off.")
				return
			}

			if !warned && silence >= watchdogWarnAfter {
				warned = true
				log.Printf("watchdog: otto silent for %s — sending warning via toto", silence.Round(time.Second))
				h.toto.SystemMessage(ctx, chatID,
					"otto's been zoning out for five minutes. i'm watching him. if he doesn't move in another five i'll boot him and you can try again.")
			}
		}
	}
}
