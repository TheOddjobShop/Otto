//go:build unix

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"otto/internal/claude"
	"otto/internal/memory"
	"otto/internal/store"
)

func TestTotoInjectsMemoryAndLogsTurn(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{respond: "mrow"}
	dir := t.TempDir()
	core := memory.NewCore(dir, 2200, 1375)
	if err := core.Add(memory.TargetUser, "User prefers brevity."); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	toto := &Toto{
		bot:     bot,
		runner:  runner,
		session: sess,
		persona: "CAT PERSONA",
		mem:     core,
		store:   st,
	}

	toto.Reply(context.Background(), 100, "hello")

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "User prefers brevity") {
		t.Errorf("toto prompt missing injected memory: %q", runner.called[0].AppendSystemPrompt)
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "CURRENT TIME") {
		t.Errorf("toto prompt missing time block: %q", runner.called[0].AppendSystemPrompt)
	}
	if got, _ := st.SearchFTS(context.Background(), "hello", 5); len(got) == 0 {
		t.Error("toto user turn not logged")
	}
}

// TestTotoStatusPromptContainsLiteralInstruction asserts that BOTH the
// direct-address and busy-fallback per-call prompts include the "Read the
// STATE above LITERALLY" guardrail. This is what stops Toto improvising
// "pulling something from somewhere" / "offline from my side" when the
// status block actually says IDLE / BUSY.
func TestTotoStatusPromptContainsLiteralInstruction(t *testing.T) {
	const want = "Read the STATE above LITERALLY"

	// Direct-address: ottoStatus is wired, snapshot returns IDLE.
	t.Run("direct-address", func(t *testing.T) {
		bot := &fakeBot{}
		runner := &fakeRunner{respond: "mrow"}
		dir := t.TempDir()
		sess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
		if err != nil {
			t.Fatal(err)
		}
		toto := &Toto{
			bot:        bot,
			runner:     runner,
			session:    sess,
			persona:    "CAT PERSONA",
			ottoStatus: func() ottoSnapshot { return ottoSnapshot{Busy: false} },
		}
		toto.Reply(context.Background(), 100, "toto what's otto doing")
		if len(runner.called) != 1 {
			t.Fatalf("runner called %d times, want 1", len(runner.called))
		}
		got := runner.called[0].AppendSystemPrompt
		if !strings.Contains(got, want) {
			t.Errorf("direct-address prompt missing %q:\n%s", want, got)
		}
		if !strings.Contains(got, "Otto is IDLE") {
			t.Errorf("direct-address prompt missing idle line:\n%s", got)
		}
	})

	// Busy-fallback: BusyReply path with an ottoPrompt+snippet.
	t.Run("busy-fallback", func(t *testing.T) {
		bot := &fakeBot{}
		runner := &fakeRunner{respond: "mrow"}
		dir := t.TempDir()
		sess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
		if err != nil {
			t.Fatal(err)
		}
		toto := &Toto{
			bot:     bot,
			runner:  runner,
			session: sess,
			persona: "CAT PERSONA",
		}
		toto.BusyReply(context.Background(), 100, "hey", "summarize my inbox", "scanning gmail...")
		if len(runner.called) != 1 {
			t.Fatalf("runner called %d times, want 1", len(runner.called))
		}
		got := runner.called[0].AppendSystemPrompt
		if !strings.Contains(got, want) {
			t.Errorf("busy-fallback prompt missing %q:\n%s", want, got)
		}
	})
}
