package store

import (
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
