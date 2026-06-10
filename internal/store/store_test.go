package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenCreatesSchema(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for _, name := range []string{"turns", "turns_fts"} {
		var got string
		err := s.db.QueryRow(
			`SELECT name FROM sqlite_master WHERE name = ?`, name,
		).Scan(&got)
		if err != nil {
			t.Fatalf("expected table %q to exist: %v", name, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open on existing db: %v", err)
	}
	s2.Close()
}

func TestAppendAndSearch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.AppendTurn(ctx, "otto", "user", "what is my flight to Tokyo"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if _, err := s.AppendTurn(ctx, "otto", "assistant", "your flight to Tokyo departs at 9am"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	if _, err := s.AppendTurn(ctx, "toto", "assistant", "mrow, otto is busy"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}

	got, err := s.SearchFTS(ctx, "Tokyo", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 matches for Tokyo, got %d: %+v", len(got), got)
	}
	for _, turn := range got {
		if turn.ID == 0 || turn.Persona == "" || turn.Content == "" {
			t.Errorf("incomplete turn returned: %+v", turn)
		}
	}
}

func TestSearchHandlesSpecialChars(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	if _, err := s.AppendTurn(ctx, "otto", "user", "error code E-42 happened"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	got, err := s.SearchFTS(ctx, `E-42 "weird) (query`, 10)
	if err != nil {
		t.Fatalf("SearchFTS with special chars must not error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 match, got %d", len(got))
	}
}

func TestSearchEmptyQueryReturnsNothing(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	got, err := s.SearchFTS(context.Background(), "   ", 10)
	if err != nil {
		t.Fatalf("SearchFTS: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("blank query should return no rows, got %d", len(got))
	}
}

// TestPruneTurnsKeepsNewest verifies that PruneTurns deletes the oldest rows
// while preserving exactly the keep most-recent ones.
func TestPruneTurnsKeepsNewest(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	var lastID int64
	for i := 0; i < 5; i++ {
		id, err := s.AppendTurn(ctx, "otto", "user", "msg")
		if err != nil {
			t.Fatalf("AppendTurn %d: %v", i, err)
		}
		lastID = id
	}

	n, err := s.PruneTurns(ctx, 3)
	if err != nil {
		t.Fatalf("PruneTurns: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 rows deleted, got %d", n)
	}

	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 turns remaining, got %d", count)
	}

	// The last inserted id must still be present (newest rows are kept).
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns WHERE id = ?`, lastID).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists != 1 {
		t.Fatalf("newest turn id=%d should survive pruning", lastID)
	}
}

// TestPruneTurnsCascadesToVectors confirms the ON DELETE CASCADE keeps the
// vectors table in sync: pruned turns must not leave orphaned vector rows.
func TestPruneTurnsCascadesToVectors(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	// Seed 4 turns each with a vector.
	for i := 0; i < 4; i++ {
		id, err := s.AppendTurn(ctx, "otto", "user", "msg")
		if err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
		if err := s.PutVector(ctx, id, "m", []float32{float32(i), 0}); err != nil {
			t.Fatalf("PutVector: %v", err)
		}
	}

	if _, err := s.PruneTurns(ctx, 2); err != nil {
		t.Fatalf("PruneTurns: %v", err)
	}

	var vecCount, turnCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns`).Scan(&turnCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM vectors`).Scan(&vecCount); err != nil {
		t.Fatal(err)
	}
	if turnCount != 2 {
		t.Fatalf("expected 2 turns, got %d", turnCount)
	}
	if vecCount != 2 {
		t.Fatalf("expected 2 vectors after cascade, got %d", vecCount)
	}
}

// TestPruneTurnsCleansUpFTS confirms the turns_ad trigger removes pruned rows
// from the FTS5 index so keyword search never returns ghost results.
func TestPruneTurnsCleansUpFTS(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if _, err := s.AppendTurn(ctx, "otto", "user", "unique keyword canary"); err != nil {
		t.Fatalf("AppendTurn: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := s.AppendTurn(ctx, "otto", "user", "other content"); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	// Prune keeping only the 3 newest; the canary is the oldest and must vanish.
	if _, err := s.PruneTurns(ctx, 3); err != nil {
		t.Fatalf("PruneTurns: %v", err)
	}

	got, err := s.SearchFTS(ctx, "canary", 10)
	if err != nil {
		t.Fatalf("SearchFTS after prune: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("pruned turn should not appear in FTS results, got %d", len(got))
	}
}

// TestPruneTurnsNoOpOnZeroKeep ensures a keep ≤ 0 leaves the table untouched.
func TestPruneTurnsNoOpOnZeroKeep(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.AppendTurn(ctx, "otto", "user", "msg"); err != nil {
			t.Fatalf("AppendTurn: %v", err)
		}
	}

	n, err := s.PruneTurns(ctx, 0)
	if err != nil {
		t.Fatalf("PruneTurns(keep=0): %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows deleted for keep=0, got %d", n)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turns`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 turns intact, got %d", count)
	}
}
