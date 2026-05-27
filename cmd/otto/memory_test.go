//go:build unix

package main

import (
	"context"
	"strings"
	"testing"

	"otto/internal/memory"
	"otto/internal/store"
)

func TestComposeMemoryPromptNilCoreReturnsBase(t *testing.T) {
	if got := composeMemoryPrompt("BASE", nil); got != "BASE" {
		t.Fatalf("nil core should return base unchanged, got %q", got)
	}
}

func TestComposeMemoryPromptEmptyCoreReturnsBase(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if got := composeMemoryPrompt("BASE", c); got != "BASE" {
		t.Fatalf("empty core should return base unchanged, got %q", got)
	}
}

func TestComposeMemoryPromptAppendsCore(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetUser, "User is named Justin."); err != nil {
		t.Fatal(err)
	}
	got := composeMemoryPrompt("BASE PROMPT", c)
	if !strings.HasPrefix(got, "BASE PROMPT") {
		t.Errorf("base should come first: %q", got)
	}
	if !strings.Contains(got, "Justin") {
		t.Errorf("memory block missing: %q", got)
	}
}

func TestComposeMemoryPromptEmptyBaseReturnsBlockOnly(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetMemory, "Server runs Arch."); err != nil {
		t.Fatal(err)
	}
	got := composeMemoryPrompt("", c)
	if strings.HasPrefix(got, "\n") {
		t.Errorf("empty base should not leave a leading separator: %q", got)
	}
	if !strings.Contains(got, "Arch") {
		t.Errorf("memory block missing: %q", got)
	}
}

func TestLogTurnPersistsAndIsSearchable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, "otto", "user", "remember the Tokyo trip")
	turns, err := st.SearchFTS(ctx, "Tokyo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 logged turn, got %d", len(turns))
	}
}

func TestLogTurnNilStoreIsNoop(t *testing.T) {
	logTurn(context.Background(), nil, "otto", "user", "anything")
}

func TestLogTurnSkipsBlankContent(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, "otto", "user", "   ")
	turns, err := st.SearchFTS(ctx, "anything", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("blank content should not be logged, got %d turns", len(turns))
	}
}
