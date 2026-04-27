package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSessionExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	if err := os.WriteFile(path, []byte("abc-123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "abc-123" {
		t.Errorf("ID = %q, want abc-123", s.ID())
	}
}

func TestLoadSessionEmptyIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "session_id")
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "" {
		t.Errorf("ID = %q, want empty", s.ID())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file should not exist when no Set has happened: %v", err)
	}
}

func TestLoadSessionEmptyIfFileEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	if err := os.WriteFile(path, []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "" {
		t.Errorf("ID = %q, want empty for empty file", s.ID())
	}
}

func TestSessionSetPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("xyz-456"); err != nil {
		t.Fatal(err)
	}
	if s.ID() != "xyz-456" {
		t.Errorf("ID = %q, want xyz-456", s.ID())
	}
	got, _ := os.ReadFile(path)
	if strings.TrimSpace(string(got)) != "xyz-456" {
		t.Errorf("file %q != xyz-456", got)
	}
}

func TestSessionSetRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	s, _ := LoadSession(path)
	if err := s.Set(""); err == nil {
		t.Error("expected error setting empty ID")
	}
}

func TestSessionClearRemovesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	if err := os.WriteFile(path, []byte("abc-123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Fatal(err)
	}
	if s.ID() != "" {
		t.Errorf("ID = %q after Clear, want empty", s.ID())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("file still present after Clear: %v", err)
	}
}

func TestSessionClearNoOpOnEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	s, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		t.Errorf("Clear on empty session returned error: %v", err)
	}
}
