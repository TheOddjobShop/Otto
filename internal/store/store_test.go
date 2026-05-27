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
