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

	id1, err := s.Enqueue(ctx, "otto", "agent", "toto", "forwarded thing", 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := s.Enqueue(ctx, "toot", "user", "", "hello toot", 0)
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
	if got[0].Hop != 1 {
		t.Errorf("row 0 Hop=%d, want 1", got[0].Hop)
	}
	if got[1].Hop != 0 {
		t.Errorf("row 1 Hop=%d, want 0", got[1].Hop)
	}
	if got[0].TS.IsZero() {
		t.Errorf("row 0 TS should have been parsed")
	}
}

func TestInboxDequeueClears(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	if _, err := s.Enqueue(ctx, "otto", "user", "", "first", 0); err != nil {
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
		if _, err := s.Enqueue(ctx, tc.target, tc.source, tc.sender, tc.body, 0); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}
}

func TestInboxDequeueCap(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	const inserted = inboxDequeueCap + 10
	for i := 0; i < inserted; i++ {
		if _, err := s.Enqueue(ctx, "otto", "user", "", "msg", 0); err != nil {
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

// TestEnqueueRejectsHopOverMax confirms the hop cap fires at MaxBusHop+1.
// MCP tool handlers turn this into a model-readable refusal so the bus
// chain stops cleanly.
func TestEnqueueRejectsHopOverMax(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()
	if _, err := s.Enqueue(ctx, "toto", "agent", "otto", "ping", MaxBusHop+1); !errors.Is(err, ErrBusHopExceeded) {
		t.Fatalf("expected ErrBusHopExceeded, got %v", err)
	}
	// Sanity: hop exactly at the cap still goes through.
	if _, err := s.Enqueue(ctx, "toto", "agent", "otto", "ping", MaxBusHop); err != nil {
		t.Fatalf("hop=MaxBusHop should succeed, got %v", err)
	}
}

// TestDequeueAllReturnsHop ensures the hop column round-trips through the
// scan path. Dispatcher relies on this so it can compose the per-call
// "HOPS REMAINING: N" line accurately.
func TestDequeueAllReturnsHop(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()
	if _, err := s.Enqueue(ctx, "toto", "agent", "otto", "ping", 2); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	got, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}
	if len(got) != 1 || got[0].Hop != 2 {
		t.Fatalf("expected one row with Hop=2, got %+v", got)
	}
}

// TestBusHopCtxRoundtrip locks in the WithBusHop / BusHopFromCtx contract.
// The dispatcher writes the count; tool handlers read it and pass hop+1
// back into Enqueue.
func TestBusHopCtxRoundtrip(t *testing.T) {
	ctx := WithBusHop(context.Background(), 2)
	n, ok := BusHopFromCtx(ctx)
	if !ok || n != 2 {
		t.Fatalf("BusHopFromCtx = (%d, %v), want (2, true)", n, ok)
	}
	if _, ok := BusHopFromCtx(context.Background()); ok {
		t.Errorf("plain ctx should report no hop counter")
	}
}

// TestLegacyAgentHopStillTripsCap exercises the back-compat shim so older
// call sites that used WithAgentHop continue to refuse nested enqueues.
func TestLegacyAgentHopStillTripsCap(t *testing.T) {
	s := newInboxStore(t)
	ctx := WithAgentHop(context.Background())
	n, _ := BusHopFromCtx(ctx)
	if _, err := s.Enqueue(ctx, "toto", "agent", "otto", "ping", n+1); !errors.Is(err, ErrBusLoopGuard) {
		t.Fatalf("expected ErrBusLoopGuard from legacy hop ctx, got %v", err)
	}
}

// TestPruneInboxRemovesDelivered verifies that PruneInbox deletes old
// delivered rows while leaving undelivered ones untouched.
func TestPruneInboxRemovesDelivered(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	// Enqueue and dequeue 5 rows so they become delivered=1.
	for i := 0; i < 5; i++ {
		if _, err := s.Enqueue(ctx, "otto", "user", "", "msg", 0); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}
	if _, err := s.DequeueAll(ctx); err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}

	// Enqueue 2 more undelivered rows.
	for i := 0; i < 2; i++ {
		if _, err := s.Enqueue(ctx, "otto", "user", "", "undelivered", 0); err != nil {
			t.Fatalf("Enqueue undelivered %d: %v", i, err)
		}
	}

	// Prune keeping the newest 2 delivered rows (so 3 old delivered rows go).
	n, err := s.PruneInbox(ctx, 2)
	if err != nil {
		t.Fatalf("PruneInbox: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 rows pruned, got %d", n)
	}

	// Undelivered rows must still be present.
	var undelivered int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE delivered = 0`).Scan(&undelivered); err != nil {
		t.Fatal(err)
	}
	if undelivered != 2 {
		t.Fatalf("expected 2 undelivered rows intact, got %d", undelivered)
	}
}

// TestPruneInboxNoOpOnZeroKeep ensures keep ≤ 0 leaves the table untouched.
func TestPruneInboxNoOpOnZeroKeep(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.Enqueue(ctx, "otto", "user", "", "msg", 0); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if _, err := s.DequeueAll(ctx); err != nil {
		t.Fatalf("DequeueAll: %v", err)
	}

	n, err := s.PruneInbox(ctx, 0)
	if err != nil {
		t.Fatalf("PruneInbox(keep=0): %v", err)
	}
	if n != 0 {
		t.Fatalf("expected 0 rows deleted for keep=0, got %d", n)
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE delivered = 1`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("expected 3 delivered rows intact, got %d", count)
	}
}
