//go:build unix

package main

import (
	"path/filepath"
	"testing"
	"time"

	"otto/internal/claude"
)

func newTestSession(t *testing.T, name, id string) *claude.Session {
	t.Helper()
	s, err := claude.LoadSession(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		if err := s.Set(id); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestTotoRotateIfIdle(t *testing.T) {
	toto := &Toto{session: newTestSession(t, "toto-sid", "sess-1")}

	// Never used (zero lastActive): must not clear even with a live session.
	toto.rotateIfIdle(15 * time.Minute)
	if toto.session.ID() == "" {
		t.Fatal("cleared a session that was never used (zero lastActive)")
	}

	// Recently active: must not clear.
	toto.lastActive = time.Now()
	toto.rotateIfIdle(15 * time.Minute)
	if toto.session.ID() == "" {
		t.Fatal("cleared a recently-active session")
	}

	// Idle past the window: must clear.
	toto.lastActive = time.Now().Add(-20 * time.Minute)
	toto.rotateIfIdle(15 * time.Minute)
	if toto.session.ID() != "" {
		t.Fatalf("did not clear an idle session; id=%q", toto.session.ID())
	}
}

func TestTootRotateIfIdle(t *testing.T) {
	toot := &Toot{session: newTestSession(t, "toot-sid", "sess-1")}

	toot.lastActive = time.Now()
	toot.rotateIfIdle(15 * time.Minute)
	if toot.session.ID() == "" {
		t.Fatal("cleared a recently-active toot session")
	}

	toot.lastActive = time.Now().Add(-20 * time.Minute)
	toot.rotateIfIdle(15 * time.Minute)
	if toot.session.ID() != "" {
		t.Fatalf("did not clear an idle toot session; id=%q", toot.session.ID())
	}
}

func TestRotateIfIdleEmptySessionNoop(t *testing.T) {
	// Empty session + old lastActive: nothing to clear, must not error/panic.
	toto := &Toto{session: newTestSession(t, "toto-sid", "")}
	toto.lastActive = time.Now().Add(-1 * time.Hour)
	toto.rotateIfIdle(15 * time.Minute)
	if toto.session.ID() != "" {
		t.Fatal("empty session should stay empty")
	}
}
