//go:build unix

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/store"
)

// busTestHandler wires a minimal handler with a real store so the drain
// loop can roundtrip rows. The fakeRunner answers every Otto/Toto/Toot
// turn with a canned reply, which is enough to assert dispatch happened.
func busTestHandler(t *testing.T) (*handler, *fakeBot, *fakeRunner, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	bot := &fakeBot{}
	runner := &fakeRunner{respond: "ok"}

	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	totoSess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	tootSess, err := claude.LoadSession(filepath.Join(dir, "toot-sid"))
	if err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	toto := &Toto{
		bot:     bot,
		runner:  runner,
		session: totoSess,
		persona: "test toto",
	}
	toot := &Toot{
		bot:     bot,
		runner:  runner,
		session: tootSess,
		persona: "test toot",
	}

	h := &handler{
		bot:       bot,
		allow:     auth.New(42),
		session:   sess,
		runner:    runner,
		startedAt: time.Now(),
		otto:      newOttoState(),
		toto:      toto,
		store:     st,
		pets:      newPetRegistry(toto, toot),
	}
	return h, bot, runner, st
}

// TestDispatchBusMessageToOtto enqueues a user→otto row and asserts the
// banner is sent and Otto's runner is invoked with the body.
func TestDispatchBusMessageToOtto(t *testing.T) {
	h, bot, runner, _ := busTestHandler(t)
	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 1, Target: "otto", Source: "user", Sender: "", Body: "hello otto",
	}
	h.dispatchBusMessage(ctx, msg)

	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sent) == 0 {
		t.Fatal("expected at least the banner to be sent")
	}
	if !strings.Contains(bot.sent[0].text, "forwarded from user") || !strings.Contains(bot.sent[0].text, "hello otto") {
		t.Errorf("banner mismatch: %q", bot.sent[0].text)
	}
	// fakeRunner records each Run call's args; expect one for Otto's turn.
	if len(runner.called) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.called))
	}
	if runner.called[0].Prompt != "hello otto" {
		t.Errorf("runner prompt mismatch: %q", runner.called[0].Prompt)
	}
}

// TestDispatchBusMessageToToto asserts a toto-targeted row triggers
// the banner + Toto's runner.
func TestDispatchBusMessageToToto(t *testing.T) {
	h, bot, runner, _ := busTestHandler(t)
	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 2, Target: "toto", Source: "agent", Sender: "otto", Body: "hey toto",
	}
	h.dispatchBusMessage(ctx, msg)

	bot.mu.Lock()
	hasBanner := false
	for _, s := range bot.sent {
		if strings.Contains(s.text, "otto → toto") && strings.Contains(s.text, "hey toto") {
			hasBanner = true
			break
		}
	}
	bot.mu.Unlock()
	if !hasBanner {
		t.Errorf("expected otto→toto banner; sent=%+v", bot.sent)
	}
	if len(runner.called) == 0 {
		t.Fatal("expected toto runner to be invoked")
	}
}

// TestDispatchBusMessageToToot asserts a toot-targeted row triggers
// the banner + Toot's runner via the pet registry lookup.
func TestDispatchBusMessageToToot(t *testing.T) {
	h, bot, runner, _ := busTestHandler(t)
	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 3, Target: "toot", Source: "agent", Sender: "otto", Body: "psst toot",
	}
	h.dispatchBusMessage(ctx, msg)

	bot.mu.Lock()
	hasBanner := false
	for _, s := range bot.sent {
		if strings.Contains(s.text, "otto → toot") {
			hasBanner = true
			break
		}
	}
	bot.mu.Unlock()
	if !hasBanner {
		t.Errorf("expected otto→toot banner; sent=%+v", bot.sent)
	}
	if len(runner.called) == 0 {
		t.Fatal("expected toot runner to be invoked")
	}
}

// TestRunBusDrainDispatchesEnqueuedRows enqueues a row directly, runs the
// drain for one tick by overriding busDrainInterval, and asserts the row
// is consumed end-to-end.
func TestRunBusDrainDispatchesEnqueuedRows(t *testing.T) {
	h, bot, _, st := busTestHandler(t)

	// Crank the interval so the test completes in well under a second.
	oldInterval := busDrainInterval
	busDrainInterval = 5 * time.Millisecond
	t.Cleanup(func() { busDrainInterval = oldInterval })

	if _, err := st.Enqueue(context.Background(), "toto", "user", "", "ping"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.runBusDrain(ctx)
		close(done)
	}()
	<-done

	bot.mu.Lock()
	defer bot.mu.Unlock()
	if len(bot.sent) == 0 {
		t.Fatal("expected drain to send the banner + a toto reply")
	}
	// Confirm the row was marked delivered (a second DequeueAll yields none).
	remaining, err := st.DequeueAll(context.Background())
	if err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("expected drain to clear inbox, got %d rows", len(remaining))
	}
}

// TestAgentHopGuardRejectsNestedEnqueue is the loop-guard contract test:
// when dispatch wraps the ctx via store.WithAgentHop, any downstream
// Enqueue call must fail with store.ErrBusLoopGuard. This is the
// mechanism PR-C/D will rely on to safely no-op on nested forwards.
func TestAgentHopGuardRejectsNestedEnqueue(t *testing.T) {
	_, _, _, st := busTestHandler(t)
	ctx := store.WithAgentHop(context.Background())
	_, err := st.Enqueue(ctx, "toto", "agent", "otto", "nested")
	if !errors.Is(err, store.ErrBusLoopGuard) {
		t.Fatalf("expected ErrBusLoopGuard under agent-hop ctx, got %v", err)
	}
}

// TestBusBannerPreviewTruncates checks the 80-rune cap behavior.
func TestBusBannerPreviewTruncates(t *testing.T) {
	long := strings.Repeat("x", 200)
	got := busBanner(store.InboxMsg{Target: "toto", Source: "user", Body: long})
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis on overlong body, got %q", got)
	}
	short := "hi"
	got = busBanner(store.InboxMsg{Target: "toto", Source: "user", Body: short})
	if strings.HasSuffix(got, "…") {
		t.Errorf("short body should not be ellipsised, got %q", got)
	}
}
