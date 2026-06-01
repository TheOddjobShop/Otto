package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func newInboxStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestInboxEnqueueDequeueRoundtrip(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	id1, err := s.Enqueue(ctx, "otto", "agent", "toto", "forwarded thing")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := s.Enqueue(ctx, "toot", "user", "", "hello toot")
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id1 == 0 || id2 == 0 || id1 == id2 {
		t.Fatalf("expected unique non-zero ids, got id1=%d id2=%d", id1, id2)
	}

	got, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 messages, got %d: %+v", len(got), got)
	}
	if got[0].ID != id1 || got[1].ID != id2 {
		t.Errorf("expected id-ordered rows, got %d then %d", got[0].ID, got[1].ID)
	}
	if got[0].Target != "otto" || got[0].Source != "agent" || got[0].Sender != "toto" || got[0].Body != "forwarded thing" {
		t.Errorf("row 0 round-trip mismatch: %+v", got[0])
	}
	if got[0].TS.IsZero() {
		t.Errorf("row 0 TS should have been parsed")
	}
}

func TestInboxDequeueClears(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	if _, err := s.Enqueue(ctx, "otto", "user", "", "first"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	first, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll #1: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("expected 1 message, got %d", len(first))
	}
	second, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll #2: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("expected drained inbox, got %d", len(second))
	}
}

func TestInboxValidationErrors(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	cases := []struct {
		name                   string
		target, source, sender string
		body                   string
	}{
		{"bad target", "ottoX", "user", "", "hi"},
		{"bad source", "otto", "system", "", "hi"},
		{"empty body", "otto", "user", "", "   "},
	}
	for _, tc := range cases {
		if _, err := s.Enqueue(ctx, tc.target, tc.source, tc.sender, tc.body); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestInboxDequeueCap(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	const inserted = inboxDequeueCap + 10
	for i := 0; i < inserted; i++ {
		if _, err := s.Enqueue(ctx, "otto", "user", "", "msg"); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	first, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll #1: %v", err)
	}
	if len(first) != inboxDequeueCap {
		t.Fatalf("expected first drain capped at %d, got %d", inboxDequeueCap, len(first))
	}
	second, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll #2: %v", err)
	}
	if len(second) != inserted-inboxDequeueCap {
		t.Fatalf("expected remainder %d, got %d", inserted-inboxDequeueCap, len(second))
	}
}

func TestInboxAgentHopGuard(t *testing.T) {
	s := newInboxStore(t)
	ctx := WithAgentHop(context.Background())

	_, err := s.Enqueue(ctx, "toto", "agent", "otto", "ping")
	if !errors.Is(err, ErrBusLoopGuard) {
		t.Fatalf("expected ErrBusLoopGuard, got %v", err)
	}
	// Sanity: undecorated context still works.
	if _, err := s.Enqueue(context.Background(), "toto", "agent", "otto", "ping"); err != nil {
		t.Fatalf("plain ctx enqueue should succeed: %v", err)
	}
}
