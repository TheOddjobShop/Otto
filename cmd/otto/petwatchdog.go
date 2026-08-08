//go:build unix

package main

import (
	"context"
	"log"
	"sync"
	"time"
)

// Pet liveness supervision.
//
// runWatchdog observes ottoState only, which left Toto and Toot unsupervised.
// That gap compounds badly, because a pet holds its own mutex for the entire
// Claude subprocess run:
//
//   - A wedged Toto never releases t.mu, so every subsequent busy-fallback
//     blocks forever in replyWithContext. Each one is a dispatch goroutine that
//     never returns, so they pile up, and the user gets silence during exactly
//     the window Toto exists to cover.
//   - Worse, Toto is the *messenger* for Otto's own watchdog. SystemMessage
//     also takes t.mu, so a wedged Toto swallows the "otto wedged, i rebooted
//     him" notification too — one hang silently disables the reporting path for
//     another.
//
// Bounding a pet turn fixes both: the mutex is released within
// petWatchdogCancelAfter no matter what the subprocess does.
const (
	// petWatchdogTick is how often pet liveness is evaluated.
	petWatchdogTick = 30 * time.Second

	// petWatchdogCancelAfter is the silence threshold for killing a pet's
	// subprocess. Far tighter than Otto's 10 minutes because the pets are a
	// different kind of workload: Haiku, a small prompt, three allowed tools,
	// and no filesystem or shell. A healthy pet turn is seconds, so two minutes
	// is already well past any legitimate slow path.
	//
	// There is deliberately no warn stage. Otto's watchdog warns *through* Toto;
	// a warning about Toto has no messenger, and a killed pet turn already
	// surfaces as that pet's in-voice fallback message, which is the same
	// information in the register the user expects.
	petWatchdogCancelAfter = 2 * time.Minute
)

// petLiveness tracks one pet's in-flight turn. Pets embed it as a field and
// expose it through supervisedPet.
//
// Lock ordering: a pet's own mu is always acquired before its petLiveness mu,
// and the watchdog only ever takes petLiveness mu. There is no path that takes
// them in the other order, so the pair cannot deadlock.
type petLiveness struct {
	mu        sync.Mutex
	busy      bool
	lastEvent time.Time
	cancel    context.CancelFunc
}

// begin marks a turn in flight and registers its cancel func.
func (l *petLiveness) begin(cancel context.CancelFunc) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.busy = true
	l.lastEvent = time.Now()
	l.cancel = cancel
}

// markEvent records that the pet's subprocess is still emitting. Called from
// the stream consumer, so a slow-but-alive turn is never killed.
func (l *petLiveness) markEvent() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.busy {
		l.lastEvent = time.Now()
	}
}

// end clears the in-flight turn. Safe to call twice (deferred alongside the
// context's own cancel).
func (l *petLiveness) end() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.busy = false
	l.cancel = nil
}

// cancelIfSilent kills the in-flight turn when it has been quiet for at least
// threshold, returning whether it cancelled and how long the silence was.
//
// Unlike ottoState.cancelInflight there is no generation counter here, because
// there is nothing to guard against: observe and act happen inside one critical
// section, so the state cannot change in between. Otto's watchdog needs the
// counter only because it sends Telegram messages between the two.
func (l *petLiveness) cancelIfSilent(threshold time.Duration) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.busy || l.cancel == nil || l.lastEvent.IsZero() {
		return false, 0
	}
	silence := time.Since(l.lastEvent)
	if silence < threshold {
		return false, silence
	}
	l.cancel()
	// Leave busy set: the turn's own deferred end() clears it once Run returns
	// from the cancellation. Clearing here would let the very next tick start
	// supervising a turn that is already being torn down.
	l.cancel = nil
	return true, silence
}

// supervisedPet is a pet whose in-flight turn the watchdog can bound. Kept
// separate from petRotator (which handles idle session clearing) because the
// two answer different questions — "is this turn stuck?" versus "is this
// session stale?" — and a pet could reasonably implement one without the other.
type supervisedPet interface {
	Name() string
	liveness() *petLiveness
}

// runPetWatchdog is a long-lived goroutine (started from main alongside the
// rotator) that bounds every registered pet's turn. Exits when ctx is
// cancelled.
//
// It is its own goroutine rather than another loop inside runRotator because
// the two cadences are unrelated: rotation is a one-minute housekeeping sweep,
// while a wedged turn should be caught on the same 30-second granularity Otto
// gets.
func runPetWatchdog(ctx context.Context, pets []supervisedPet) {
	if len(pets) == 0 {
		return
	}
	ticker := time.NewTicker(petWatchdogTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, p := range pets {
				if cancelled, silence := p.liveness().cancelIfSilent(petWatchdogCancelAfter); cancelled {
					log.Printf("pet watchdog: %s silent for %s — cancelling subprocess",
						p.Name(), silence.Round(time.Second))
				}
			}
		}
	}
}
