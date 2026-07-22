//go:build unix

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/store"
)

// newBusHandler wires a handler with a real store for bus-path tests.
func newBusHandler(t *testing.T, bot *fakeBot, runner claude.Runner) (*handler, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	totoSess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	toto := &Toto{bot: bot, runner: &fakeRunner{respond: "mrow"}, session: totoSess, persona: "cat"}
	h := &handler{
		bot: bot, allow: auth.New(99), session: sess, runner: runner,
		startedAt: time.Now(), otto: newOttoState(), toto: toto, store: st,
	}
	return h, st
}

// TestBusyOttoDefersInsteadOfDropping is the fix: a forwarded message that
// arrives while Otto is mid-turn used to be answered by Toto and then lost,
// because DequeueAll marks rows delivered before dispatch. It must survive.
func TestBusyOttoDefersInsteadOfDropping(t *testing.T) {
	bot := &fakeBot{}
	h, st := newBusHandler(t, bot, &fakeRunner{respond: "ok"})
	ctx := context.Background()

	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "check my email", 1); err != nil {
		t.Fatal(err)
	}
	batch, err := st.DequeueAll(ctx)
	if err != nil || len(batch) != 1 {
		t.Fatalf("dequeue: %v %d", err, len(batch))
	}

	// Otto is busy on something else.
	if !h.otto.tryAcquire("a long coding task") {
		t.Fatal("could not take the slot")
	}
	h.dispatchBusMessage(ctx, batch[0])

	// The message must be back in the queue, not gone. The attempt counter is
	// the discriminator: a deferral bumps it to 1, whereas a dropped row is
	// still at 0. Defer returns the post-increment value, so a deferred row
	// reports 2 here and a dropped one would report 1 — asserted through the
	// public API rather than by reaching into the database.
	_, attempts, err := st.Defer(ctx, batch[0].ID, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 (dispatcher deferred, then this call); "+
			"1 would mean the dispatcher dropped the message", attempts)
	}
	// Still deliverable.
	back, err := st.DequeueAll(ctx)
	if err != nil || len(back) != 1 {
		t.Fatalf("deferred message did not survive: %v %d", err, len(back))
	}

	// And Toto covered for Otto exactly once.
	if n := len(bot.sent); n != 1 {
		t.Errorf("sent %d messages, want 1 (Toto's cover)", n)
	}
}

// TestDeferredMessageIsDeliveredOnceOttoFrees — the whole user-visible point:
// "tell that to otto once he is done".
func TestDeferredMessageIsDeliveredOnceOttoFrees(t *testing.T) {
	bot := &fakeBot{}
	ottoRunner := &fakeRunner{respond: "on it"}
	h, st := newBusHandler(t, bot, ottoRunner)
	ctx := context.Background()

	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "check my email", 1); err != nil {
		t.Fatal(err)
	}
	batch, _ := st.DequeueAll(ctx)

	h.otto.tryAcquire("busy")
	h.dispatchBusMessage(ctx, batch[0])
	h.otto.release() // Otto finishes

	// Make it due now rather than waiting out busDeferDelay.
	if _, _, err := st.Defer(ctx, batch[0].ID, -time.Second); err != nil {
		t.Fatal(err)
	}
	redelivered, err := st.DequeueAll(ctx)
	if err != nil || len(redelivered) != 1 {
		t.Fatalf("redelivery: %v %d", err, len(redelivered))
	}
	h.dispatchBusMessage(ctx, redelivered[0])

	if len(ottoRunner.called) != 1 {
		t.Fatalf("Otto ran %d times, want 1 (the deferred message)", len(ottoRunner.called))
	}
	if !strings.Contains(ottoRunner.called[0].Prompt, "check my email") {
		t.Errorf("Otto got the wrong prompt: %q", ottoRunner.called[0].Prompt)
	}
}

// TestRepeatedDeferralsDoNotSpamTheUser — a five-minute Otto turn produces ten
// retries; ten "otto's busy" messages would read as a malfunction.
func TestRepeatedDeferralsDoNotSpamTheUser(t *testing.T) {
	bot := &fakeBot{}
	h, st := newBusHandler(t, bot, &fakeRunner{respond: "ok"})
	ctx := context.Background()

	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "ping", 1); err != nil {
		t.Fatal(err)
	}
	batch, _ := st.DequeueAll(ctx)
	h.otto.tryAcquire("busy the whole time")

	msg := batch[0]
	for i := 0; i < 5; i++ {
		h.dispatchBusMessage(ctx, msg)
		if _, _, err := st.Defer(ctx, msg.ID, -time.Second); err != nil {
			t.Fatal(err)
		}
		next, err := st.DequeueAll(ctx)
		if err != nil || len(next) != 1 {
			t.Fatalf("retry %d: %v %d", i, err, len(next))
		}
		msg = next[0]
	}

	if n := len(bot.sent); n != 1 {
		t.Errorf("user got %d messages across 5 deferrals, want exactly 1", n)
	}
}

// TestExhaustedDeliveryTellsTheUser — silence would be the worst outcome: the
// user watched Toto accept the hand-off and would assume Otto has it.
func TestExhaustedDeliveryTellsTheUser(t *testing.T) {
	bot := &fakeBot{}
	h, st := newBusHandler(t, bot, &fakeRunner{respond: "ok"})
	ctx := context.Background()

	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "summarize my inbox", 1); err != nil {
		t.Fatal(err)
	}
	batch, _ := st.DequeueAll(ctx)
	id := batch[0].ID

	// Burn the attempt budget.
	for i := 1; i < store.MaxDeliveryAttempts; i++ {
		if _, _, err := st.Defer(ctx, id, -time.Second); err != nil {
			t.Fatal(err)
		}
	}
	h.otto.tryAcquire("still busy")
	// Hand it a message carrying a non-zero attempt count, as the drain would.
	h.dispatchBusMessage(ctx, store.InboxMsg{
		ID: id, Target: "otto", Source: "agent", Sender: "toto",
		Body: "summarize my inbox", Hop: 1, Attempts: store.MaxDeliveryAttempts - 1,
	})

	var found bool
	for _, m := range bot.sent {
		if strings.Contains(m.text, "Couldn't hand this to Otto") &&
			strings.Contains(m.text, "summarize my inbox") {
			found = true
		}
	}
	if !found {
		t.Errorf("user was not told the hand-off failed; sent = %+v", bot.sent)
	}
}
