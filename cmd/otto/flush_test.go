//go:build unix

package main

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/memory"
)

func TestShouldFlush(t *testing.T) {
	cases := []struct {
		name     string
		enabled  bool
		memWired bool
		tokens   int
		want     bool
	}{
		{"typical rotation", true, true, 50000, true},
		{"exactly at floor", true, true, flushMinTokens, true},
		{"below floor", true, true, flushMinTokens - 1, false},
		{"disabled by config", false, true, 50000, false},
		{"no memory core wired", true, false, 50000, false},
		{"empty session", true, true, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldFlush(c.enabled, c.memWired, c.tokens); got != c.want {
				t.Errorf("shouldFlush(%v, %v, %d) = %v, want %v",
					c.enabled, c.memWired, c.tokens, got, c.want)
			}
		})
	}
}

// TestRunFlushCallShape pins the properties that make the flush safe to run
// unattended: it resumes the session being cleared, runs on the cheap tier, and
// can ONLY add memory — never replace or remove.
func TestRunFlushCallShape(t *testing.T) {
	runner := &fakeRunner{respond: "saved 1 fact"}
	h := newTestHandler(t, &fakeBot{}, runner)
	h.mem = memory.NewCore(t.TempDir(), 2200, 1375)

	if !h.runFlush(context.Background(), "sess-abc", 50000) {
		t.Fatal("runFlush reported failure on a successful run")
	}
	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	got := runner.called[0]

	if got.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want the session being cleared", got.SessionID)
	}
	if got.Model != flushModel {
		t.Errorf("Model = %q, want %q (flush must stay cheap)", got.Model, flushModel)
	}
	if got.Source != "flush" {
		t.Errorf("Source = %q, want \"flush\" so the cost is attributable", got.Source)
	}
	if len(got.AllowedTools) != 1 || got.AllowedTools[0] != "mcp__otto-memory__memory_add" {
		t.Errorf("AllowedTools = %v, want memory_add only", got.AllowedTools)
	}
	for _, forbidden := range []string{"memory_replace", "memory_remove"} {
		if strings.Contains(strings.Join(got.AllowedTools, ","), forbidden) {
			t.Errorf("flush must not be able to call %s", forbidden)
		}
	}
	// The core is injected so the pass can skip what's already stored.
	if !strings.Contains(got.AppendSystemPrompt, "background memory-flush pass") {
		t.Errorf("system prompt missing flush framing:\n%s", got.AppendSystemPrompt)
	}
}

// TestRunFlushFailureIsSwallowed — a broken flush must never block the
// rotation it precedes.
func TestRunFlushFailureIsSwallowed(t *testing.T) {
	runner := &fakeRunner{failErr: context.DeadlineExceeded}
	h := newTestHandler(t, &fakeBot{}, runner)
	h.mem = memory.NewCore(t.TempDir(), 2200, 1375)

	if h.runFlush(context.Background(), "sess-abc", 50000) {
		t.Error("runFlush should report failure when the runner errors")
	}
	// No panic, no propagation — the caller continues to Clear().
}

func TestRunFlushNoSessionIsNoop(t *testing.T) {
	runner := &fakeRunner{respond: "x"}
	h := newTestHandler(t, &fakeBot{}, runner)
	if h.runFlush(context.Background(), "", 50000) {
		t.Error("runFlush on an empty session id should not run")
	}
	if len(runner.called) != 0 {
		t.Error("runFlush must not spawn a subprocess without a session")
	}
}

// flushTrackingRunner records the order of Run calls so a test can assert the
// flush happened while the session still existed.
type flushTrackingRunner struct {
	mu        sync.Mutex
	sessionAt []string // session id observed on each Run
	sessionOf func() string
}

func (r *flushTrackingRunner) Run(ctx context.Context, args claude.RunArgs) error {
	r.mu.Lock()
	// Observe the live session file at the moment the flush runs.
	r.sessionAt = append(r.sessionAt, r.sessionOf())
	r.mu.Unlock()
	args.Events <- claude.ResultEvent{Subtype: "success"}
	return nil
}

func (r *flushTrackingRunner) WithEnv(map[string]string) claude.Runner { return r }

// TestMaybeRotateFlushesBeforeClearing is the ordering property: distilling a
// session that has already been cleared would resume nothing.
func TestMaybeRotateFlushesBeforeClearing(t *testing.T) {
	dir := t.TempDir()
	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Set("sess-live"); err != nil {
		t.Fatal(err)
	}

	runner := &flushTrackingRunner{sessionOf: sess.ID}
	h := &handler{
		bot:     &fakeBot{},
		allow:   auth.New(99),
		session: sess,
		runner:  runner,
		otto:    newOttoState(),
		mem:     memory.NewCore(dir, 2200, 1375),
		rotate: rotateConfig{
			ctxTokens:  200000,
			hard:       0.85,
			idleWindow: time.Millisecond,
			flush:      true,
		},
	}
	h.otto.setInputTokens(50000)
	h.otto.markUserMessage()
	time.Sleep(5 * time.Millisecond) // exceed the idle window

	h.maybeRotate(context.Background())

	if len(runner.sessionAt) != 1 {
		t.Fatalf("flush ran %d times, want 1", len(runner.sessionAt))
	}
	if runner.sessionAt[0] != "sess-live" {
		t.Errorf("flush saw session %q, want it to run BEFORE the clear", runner.sessionAt[0])
	}
	if sess.ID() != "" {
		t.Errorf("session = %q, want cleared after the flush", sess.ID())
	}
}

// TestMaybeRotateSkipsFlushWhenDisabled — the config switch must actually gate
// the subprocess, not just the logging.
func TestMaybeRotateSkipsFlushWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Set("sess-live"); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{respond: "x"}
	h := &handler{
		bot:     &fakeBot{},
		allow:   auth.New(99),
		session: sess,
		runner:  runner,
		otto:    newOttoState(),
		mem:     memory.NewCore(dir, 2200, 1375),
		rotate: rotateConfig{
			ctxTokens:  200000,
			hard:       0.85,
			idleWindow: time.Millisecond,
			flush:      false,
		},
	}
	h.otto.setInputTokens(50000)
	h.otto.markUserMessage()
	time.Sleep(5 * time.Millisecond)

	h.maybeRotate(context.Background())

	if len(runner.called) != 0 {
		t.Errorf("flush ran despite being disabled: %v", runner.called)
	}
	if sess.ID() != "" {
		t.Error("rotation should still clear the session with flush disabled")
	}
}
