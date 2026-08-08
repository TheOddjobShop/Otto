//go:build unix

package main

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPetLivenessCancelsAfterSilence(t *testing.T) {
	var l petLiveness
	cancelled := false
	l.begin(func() { cancelled = true })

	// Backdate the last event past the threshold.
	l.mu.Lock()
	l.lastEvent = time.Now().Add(-3 * time.Minute)
	l.mu.Unlock()

	got, silence := l.cancelIfSilent(petWatchdogCancelAfter)
	if !got {
		t.Fatal("expected cancellation after silence past the threshold")
	}
	if !cancelled {
		t.Error("cancel func was not invoked")
	}
	if silence < petWatchdogCancelAfter {
		t.Errorf("reported silence %s is below the threshold %s", silence, petWatchdogCancelAfter)
	}
}

func TestPetLivenessLeavesFreshTurnAlone(t *testing.T) {
	var l petLiveness
	cancelled := false
	l.begin(func() { cancelled = true })

	got, _ := l.cancelIfSilent(petWatchdogCancelAfter)
	if got || cancelled {
		t.Fatal("a turn that just started must not be cancelled")
	}
}

// A long turn that keeps emitting is healthy, not wedged — the watchdog
// measures silence rather than age, so markEvent must reset the clock.
func TestPetLivenessMarkEventDefersCancel(t *testing.T) {
	var l petLiveness
	cancelled := false
	l.begin(func() { cancelled = true })

	l.mu.Lock()
	l.lastEvent = time.Now().Add(-3 * time.Minute)
	l.mu.Unlock()

	l.markEvent()

	got, _ := l.cancelIfSilent(petWatchdogCancelAfter)
	if got || cancelled {
		t.Fatal("markEvent should have reset the silence clock")
	}
}

func TestPetLivenessIdleIsNotCancelled(t *testing.T) {
	var l petLiveness
	got, _ := l.cancelIfSilent(petWatchdogCancelAfter)
	if got {
		t.Fatal("a pet with no turn in flight must not report a cancellation")
	}
}

// end() must make the state inert: a tick landing between a turn finishing and
// the next one starting should find nothing to do.
func TestPetLivenessEndClearsInflight(t *testing.T) {
	var l petLiveness
	cancelled := false
	l.begin(func() { cancelled = true })
	l.end()

	l.mu.Lock()
	l.lastEvent = time.Now().Add(-3 * time.Minute)
	l.mu.Unlock()

	got, _ := l.cancelIfSilent(petWatchdogCancelAfter)
	if got || cancelled {
		t.Fatal("a finished turn must not be cancellable")
	}
}

// Cancelling must be one-shot: the turn's teardown may take a moment, and a
// second tick in that window must not fire cancel again.
func TestPetLivenessCancelsOnlyOnce(t *testing.T) {
	var l petLiveness
	calls := 0
	l.begin(func() { calls++ })

	l.mu.Lock()
	l.lastEvent = time.Now().Add(-3 * time.Minute)
	l.mu.Unlock()

	if got, _ := l.cancelIfSilent(petWatchdogCancelAfter); !got {
		t.Fatal("first cancel should have fired")
	}
	if got, _ := l.cancelIfSilent(petWatchdogCancelAfter); got {
		t.Fatal("second tick should not re-cancel a turn already being torn down")
	}
	if calls != 1 {
		t.Errorf("cancel func called %d times, want exactly 1", calls)
	}
}

// The real payoff: cancelling frees the pet's mutex. This reproduces the
// original failure — a turn holding mu while its subprocess never returns —
// and asserts that a waiter gets in once the watchdog fires.
func TestPetWatchdogUnblocksWaiterOnWedgedTurn(t *testing.T) {
	var mu sync.Mutex
	var l petLiveness

	wedged := make(chan struct{})
	released := make(chan struct{})

	go func() {
		mu.Lock()
		defer mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		l.begin(cancel)
		defer l.end()
		close(wedged)
		<-ctx.Done() // stand-in for a subprocess that never returns on its own
	}()

	<-wedged

	// A second caller (a busy-fallback, or SystemMessage) piles up behind it.
	go func() {
		mu.Lock()
		mu.Unlock()
		close(released)
	}()

	select {
	case <-released:
		t.Fatal("waiter acquired the mutex before the wedged turn was cancelled")
	case <-time.After(20 * time.Millisecond):
	}

	l.mu.Lock()
	l.lastEvent = time.Now().Add(-3 * time.Minute)
	l.mu.Unlock()
	if got, _ := l.cancelIfSilent(petWatchdogCancelAfter); !got {
		t.Fatal("watchdog did not cancel the wedged turn")
	}

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("waiter still blocked after the wedged turn was cancelled")
	}
}

// Both pets must satisfy supervisedPet, which is what main.go registers them
// as. A missing liveness() would otherwise only surface at wiring time.
func TestPetsImplementSupervisedPet(t *testing.T) {
	var _ supervisedPet = &Toto{}
	var _ supervisedPet = &Toot{}
}

func TestRunPetWatchdogExitsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runPetWatchdog(ctx, []supervisedPet{&Toto{}, &Toot{}})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runPetWatchdog did not exit on context cancellation")
	}
}

// With no pets registered the watchdog returns immediately rather than
// spinning a ticker forever — relevant to test configurations and any future
// partial wiring.
func TestRunPetWatchdogNoPetsReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		runPetWatchdog(context.Background(), nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runPetWatchdog with no pets should return immediately")
	}
}
