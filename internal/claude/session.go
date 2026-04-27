// Package claude wraps the Claude Code CLI as a subprocess and parses its
// stream-json output.
package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Session holds the current Claude Code session ID, persisted at Path so it
// survives Otto restarts. The ID is empty until Claude Code creates a real
// session — Otto captures it from the system/init event in stream-json
// output and calls Set. Pre-generating UUIDs would not work because Claude
// Code's --resume flag rejects unknown IDs.
type Session struct {
	mu   sync.RWMutex
	id   string
	path string
}

// LoadSession reads the session ID from path, returning an empty Session if
// the file is missing or empty. Caller must use Set after observing an ID
// from Claude Code's first stream-json system/init event.
func LoadSession(path string) (*Session, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("claude: ensure session dir: %w", err)
	}
	s := &Session{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, fmt.Errorf("claude: read session: %w", err)
	}
	s.id = strings.TrimSpace(string(data))
	return s, nil
}

// ID returns the current session ID, or "" if no session has been captured.
func (s *Session) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

// Path returns the on-disk location of the session ID file.
func (s *Session) Path() string { return s.path }

// Set persists a session ID. No-op if id matches the current value.
func (s *Session) Set(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "" {
		return fmt.Errorf("claude: refuse to Set empty session ID")
	}
	if s.id == id {
		return nil
	}
	if err := writeSession(s.path, id); err != nil {
		return err
	}
	s.id = id
	return nil
}

// Clear removes the persisted session ID so the next call to claude will
// start fresh (no --resume flag). Used by the /new command.
func (s *Session) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id == "" {
		return nil
	}
	if err := os.Remove(s.path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("claude: clear session: %w", err)
	}
	s.id = ""
	return nil
}

func writeSession(path, id string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(id+"\n"), 0600); err != nil {
		return fmt.Errorf("claude: write session: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("claude: rename session: %w", err)
	}
	return nil
}
