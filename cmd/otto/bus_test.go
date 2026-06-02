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

// TestDispatchBusMessageToOttoUser asserts a user-sourced row routes to
// Otto's normal handleMessage path with no BUS CONTEXT injected (it's
// just a regular Telegram message that took a detour through the inbox).
func TestDispatchBusMessageToOttoUser(t *testing.T) {
	h, bot, runner, _ := busTestHandler(t)
	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 1, Target: "otto", Source: "user", Sender: "", Body: "hello otto", Hop: 0,
	}
	h.dispatchBusMessage(ctx, msg)

	if len(runner.called) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.called))
	}
	if runner.called[0].Prompt != "hello otto" {
		t.Errorf("runner prompt mismatch: %q", runner.called[0].Prompt)
	}
	// User-sourced rows must NOT carry BUS CONTEXT into Otto's prompt.
	if strings.Contains(runner.called[0].AppendSystemPrompt, "BUS CONTEXT") {
		t.Errorf("user-sourced row leaked BUS CONTEXT into Otto's prompt: %q", runner.called[0].AppendSystemPrompt)
	}
	// And the banner must be gone — the bus is silent now.
	bot.mu.Lock()
	defer bot.mu.Unlock()
	for _, s := range bot.sent {
		if strings.Contains(s.text, "↪") || strings.Contains(s.text, "forwarded from") {
			t.Errorf("banner leaked into chat: %q", s.text)
		}
	}
}

// TestDispatchBusMessageToOttoFromAgentInjectsBusContext asserts an
// agent-sourced row to Otto runs through handleBusOttoMessage, which
// prepends a BUS CONTEXT + HOPS REMAINING block to his per-call prompt
// and stamps the bus env vars on the runner.
func TestDispatchBusMessageToOttoFromAgentInjectsBusContext(t *testing.T) {
	h, _, runner, _ := busTestHandler(t)
	// Wrap runner to capture env it gets; the existing fakeRunner's WithEnv
	// returns itself, so the env is reflected via the args.AppendSystemPrompt
	// check below. We add a dedicated env-recording runner here.
	envRec := &envRecordingRunner{fakeRunner: runner}
	h.runner = envRec

	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 2, Target: "otto", Source: "agent", Sender: "toto", Body: "yo bro", Hop: 1,
	}
	h.dispatchBusMessage(ctx, msg)

	if len(runner.called) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.called))
	}
	got := runner.called[0].AppendSystemPrompt
	for _, want := range []string{
		"BUS CONTEXT",
		"From:  toto",
		"To:    otto",
		"Hop:   1 of 3",
		"Remaining hops: 2",
		"HOPS REMAINING: 2",
		"REPLY PATH",
		"you MUST call",
		"message_toto",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("otto bus prompt missing %q:\n%s", want, got)
		}
	}
	if envRec.lastEnv["OTTO_BUS_HOP"] != "1" {
		t.Errorf("expected OTTO_BUS_HOP=1 stamped on runner env, got %q", envRec.lastEnv["OTTO_BUS_HOP"])
	}
	if envRec.lastEnv["OTTO_BUS_SENDER"] != "otto" {
		t.Errorf("expected OTTO_BUS_SENDER=otto stamped on runner env, got %q", envRec.lastEnv["OTTO_BUS_SENDER"])
	}
}

// TestDispatchBusMessageToTotoFromAgentInjectsBusContext mirrors the Otto
// path: Toto's per-call system prompt grows the BUS CONTEXT block, and
// the runner sees the bus env vars (hop matches the row, sender = toto).
func TestDispatchBusMessageToTotoFromAgentInjectsBusContext(t *testing.T) {
	h, bot, runner, _ := busTestHandler(t)
	envRec := &envRecordingRunner{fakeRunner: runner}
	h.toto.runner = envRec

	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 3, Target: "toto", Source: "agent", Sender: "otto", Body: "yo cat", Hop: 2,
	}
	h.dispatchBusMessage(ctx, msg)

	if len(runner.called) != 1 {
		t.Fatalf("expected 1 toto runner call, got %d", len(runner.called))
	}
	got := runner.called[0].AppendSystemPrompt
	for _, want := range []string{
		"BUS CONTEXT",
		"From:  otto",
		"To:    toto",
		"Hop:   2 of 3",
		"Remaining hops: 1",
		"HOPS REMAINING: 1",
		"REPLY PATH",
		"you MUST call",
		"message_otto",
	} {
		// Note: "message_otto" doesn't exist as a tool; the prompt mentions
		// message_<sender>. For sender=otto the literal string is
		// "message_otto"; the toto persona handles the no-tool case in voice.
		if !strings.Contains(got, want) {
			t.Errorf("toto bus prompt missing %q:\n%s", want, got)
		}
	}
	if envRec.lastEnv["OTTO_BUS_HOP"] != "2" {
		t.Errorf("expected OTTO_BUS_HOP=2 stamped on runner env, got %q", envRec.lastEnv["OTTO_BUS_HOP"])
	}
	if envRec.lastEnv["OTTO_BUS_SENDER"] != "toto" {
		t.Errorf("expected OTTO_BUS_SENDER=toto stamped on runner env, got %q", envRec.lastEnv["OTTO_BUS_SENDER"])
	}
	// Banner is gone.
	bot.mu.Lock()
	defer bot.mu.Unlock()
	for _, s := range bot.sent {
		if strings.Contains(s.text, "↪") || strings.Contains(s.text, "→ toto") {
			t.Errorf("banner leaked into chat: %q", s.text)
		}
	}
}

// TestDispatchBusMessageToTootFromAgentInjectsBusContext asserts the
// Toot path runs through Toot.BusReply with the BUS CONTEXT block and
// bus env vars on his runner. Toot's tool allowlist exposes message_toto
// and forward_to_otto so he can keep the loop alive in either direction.
func TestDispatchBusMessageToTootFromAgentInjectsBusContext(t *testing.T) {
	h, bot, runner, _ := busTestHandler(t)
	envRec := &envRecordingRunner{fakeRunner: runner}
	h.findToot().runner = envRec

	ctx := context.Background()
	msg := store.InboxMsg{
		ID: 4, Target: "toot", Source: "agent", Sender: "toto", Body: "psst owl", Hop: 1,
	}
	h.dispatchBusMessage(ctx, msg)

	if len(runner.called) != 1 {
		t.Fatalf("expected 1 toot runner call, got %d", len(runner.called))
	}
	got := runner.called[0].AppendSystemPrompt
	for _, want := range []string{
		"BUS CONTEXT",
		"From:  toto",
		"To:    toot",
		"Hop:   1 of 3",
		"Remaining hops: 2",
		"HOPS REMAINING: 2",
		"REPLY PATH",
		"you MUST call",
		"message_toto",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("toot bus prompt missing %q:\n%s", want, got)
		}
	}
	if envRec.lastEnv["OTTO_BUS_HOP"] != "1" {
		t.Errorf("expected OTTO_BUS_HOP=1 stamped on runner env, got %q", envRec.lastEnv["OTTO_BUS_HOP"])
	}
	if envRec.lastEnv["OTTO_BUS_SENDER"] != "toot" {
		t.Errorf("expected OTTO_BUS_SENDER=toot stamped on runner env, got %q", envRec.lastEnv["OTTO_BUS_SENDER"])
	}
	// The Toot tool allowlist should include the bus tools so he can reply.
	got = strings.Join(runner.called[0].AllowedTools, ",")
	for _, want := range []string{"message_toto", "forward_to_otto"} {
		if !strings.Contains(got, want) {
			t.Errorf("toot allowlist missing %q: %s", want, got)
		}
	}
	// Banner is gone.
	bot.mu.Lock()
	defer bot.mu.Unlock()
	for _, s := range bot.sent {
		if strings.Contains(s.text, "↪") || strings.Contains(s.text, "→ toot") {
			t.Errorf("banner leaked into chat: %q", s.text)
		}
	}
}

// TestDispatchBusLastHopWindsDown asserts that when a row arrives at the
// hop cap, the recipient's prompt instructs them not to call any bus
// tool — the chain must end. This is the "wrap things up" guidance the
// persona uses to land cleanly.
func TestDispatchBusLastHopWindsDown(t *testing.T) {
	h, _, runner, _ := busTestHandler(t)
	envRec := &envRecordingRunner{fakeRunner: runner}
	h.toto.runner = envRec

	msg := store.InboxMsg{
		ID: 5, Target: "toto", Source: "agent", Sender: "otto", Body: "last word", Hop: 3,
	}
	h.dispatchBusMessage(context.Background(), msg)

	if len(runner.called) != 1 {
		t.Fatalf("expected 1 runner call, got %d", len(runner.called))
	}
	got := runner.called[0].AppendSystemPrompt
	for _, want := range []string{
		"HOPS REMAINING: 0",
		"Remaining hops: 0",
		"REPLY PATH",
		"Do NOT call",
		"chain ends",
		"wrap up",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("last-hop prompt missing %q:\n%s", want, got)
		}
	}
	// On the last hop, we must NOT tell the model to call a bus tool —
	// otherwise the wind-down is muddied and the model may fire one anyway.
	for _, banned := range []string{"you MUST call", "Standard pattern for this turn"} {
		if strings.Contains(got, banned) {
			t.Errorf("last-hop prompt unexpectedly contains %q:\n%s", banned, got)
		}
	}
}

// TestRunBusDrainDispatchesEnqueuedRows enqueues a row directly, runs the
// drain for one tick by overriding busDrainInterval, and asserts the row
// is consumed end-to-end.
func TestRunBusDrainDispatchesEnqueuedRows(t *testing.T) {
	h, _, runner, st := busTestHandler(t)

	// Crank the interval so the test completes in well under a second.
	oldInterval := busDrainInterval
	busDrainInterval = 5 * time.Millisecond
	t.Cleanup(func() { busDrainInterval = oldInterval })

	if _, err := st.Enqueue(context.Background(), "toto", "user", "", "ping", 0); err != nil {
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

	if len(runner.called) == 0 {
		t.Fatal("expected drain to invoke toto's runner")
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

// TestHopCapRejectsAtBoundary is the store-level contract test that the
// MCP tools rely on: an Enqueue at hop = MaxBusHop+1 must fail with
// ErrBusHopExceeded so the chain stops cleanly rather than looping.
func TestHopCapRejectsAtBoundary(t *testing.T) {
	_, _, _, st := busTestHandler(t)
	if _, err := st.Enqueue(context.Background(), "toto", "agent", "otto", "ping", store.MaxBusHop+1); err == nil {
		t.Fatal("expected hop-cap rejection at MaxBusHop+1")
	}
}

// envRecordingRunner captures the last env map handed to WithEnv so
// tests can assert the dispatcher stamped the right bus context onto the
// recipient's claude subprocess.
type envRecordingRunner struct {
	*fakeRunner
	lastEnv map[string]string
}

func (e *envRecordingRunner) WithEnv(extra map[string]string) claude.Runner {
	e.lastEnv = extra
	return e
}
