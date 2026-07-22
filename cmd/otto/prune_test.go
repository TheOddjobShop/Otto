//go:build unix

package main

import (
	"context"
	"testing"

	"otto/internal/store"
)

// TestPruneStoreOnceTrimsTurns confirms the maintenance pass actually prunes
// the turn log down to the keep bound: after pruning to keep=2, a follow-up
// PruneTurns(2) finds nothing left to remove.
func TestPruneStoreOnceTrimsTurns(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	for i := 0; i < 5; i++ {
		if _, err := st.AppendTurn(ctx, "otto", "user", "turn"); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	pruneStoreOnce(ctx, st, 2, 100, 100)

	// If pruneStoreOnce trimmed to 2 rows, a second prune to the same bound
	// removes nothing.
	n, err := st.PruneTurns(ctx, 2)
	if err != nil {
		t.Fatalf("PruneTurns: %v", err)
	}
	if n != 0 {
		t.Errorf("expected 0 rows to remain prunable after pruneStoreOnce, got %d", n)
	}
}
