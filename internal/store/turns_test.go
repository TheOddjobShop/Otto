package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// newTestStore opens a throwaway store rooted in the test's temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestRecentTurnsIsChronologicalAndBounded pins the contract that makes
// recent_turns useful for anaphora: the newest N turns, but handed back
// oldest-first so the model reads them as a conversation.
func TestRecentTurnsIsChronologicalAndBounded(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if _, err := st.AppendTurn(ctx, "otto", "user", fmt.Sprintf("msg-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.RecentTurns(ctx, 3, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d turns, want 3", len(got))
	}
	// Newest three are 7,8,9 — returned oldest-first.
	for i, want := range []string{"msg-7", "msg-8", "msg-9"} {
		if got[i].Content != want {
			t.Errorf("turn %d = %q, want %q", i, got[i].Content, want)
		}
	}
	if got[0].ID >= got[2].ID {
		t.Error("turns should ascend by id (chronological)")
	}
}

func TestRecentTurnsPagesBackwards(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	for i := 0; i < 6; i++ {
		if _, err := st.AppendTurn(ctx, "otto", "user", fmt.Sprintf("m%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.RecentTurns(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	older, err := st.RecentTurns(ctx, 2, first[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 2 {
		t.Fatalf("got %d older turns, want 2", len(older))
	}
	if older[len(older)-1].ID >= first[0].ID {
		t.Errorf("beforeID did not exclude the already-seen page: %+v", older)
	}
}

func TestRecentTurnsEmptyAndZeroLimit(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if got, err := st.RecentTurns(ctx, 5, 0); err != nil || len(got) != 0 {
		t.Errorf("empty store: got %v, err %v", got, err)
	}
	if _, err := st.AppendTurn(ctx, "otto", "user", "x"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.RecentTurns(ctx, 0, 0); err != nil || got != nil {
		t.Errorf("zero limit should return nothing: got %v, err %v", got, err)
	}
}

// TestRecentTurnsPreservesPersona — the user may have been talking to Toto in
// between, and attributing a line to the wrong speaker is worse than omitting it.
func TestRecentTurnsPreservesPersona(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.AppendTurn(ctx, "toto", "assistant", "mrow"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendTurn(ctx, "otto", "assistant", "done"); err != nil {
		t.Fatal(err)
	}
	got, err := st.RecentTurns(ctx, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Persona != "toto" || got[1].Persona != "otto" {
		t.Errorf("personas = %q, %q", got[0].Persona, got[1].Persona)
	}
}
