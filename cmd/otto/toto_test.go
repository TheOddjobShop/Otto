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
