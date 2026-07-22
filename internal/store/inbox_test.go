package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

// TestEnqueueRefusesBeyondHopCap confirms that a ctx carrying a hop counter
// at MaxBusHop refuses an enqueue at hop+1, so a dispatch already at the cap
// cannot push a further forward.
func TestEnqueueRefusesBeyondHopCap(t *testing.T) {
	s := newInboxStore(t)
	ctx := WithBusHop(context.Background(), MaxBusHop)
	n, _ := BusHopFromCtx(ctx)
	if _, err := s.Enqueue(ctx, "toto", "agent", "otto", "ping", n+1); !errors.Is(err, ErrBusHopExceeded) {
		t.Fatalf("expected ErrBusHopExceeded from hop ctx at cap, got %v", err)
	}
}

// TestDequeueAllConcurrentWithEnqueue hammers DequeueAll while a writer
// goroutine enqueues on other pooled connections. DequeueAll must begin as
// a write transaction (see the no-op UPDATE it issues first); a deferred
// transaction would open a read snapshot on its SELECT and the delivered=1
// UPDATE's upgrade would fail with SQLITE_BUSY_SNAPSHOT whenever a
// concurrent Enqueue commits in between — an error busy_timeout does not
// retry.
func TestDequeueAllConcurrentWithEnqueue(t *testing.T) {
	s := newInboxStore(t)
	ctx := context.Background()

	const writes = 500
	done := make(chan error, 1)
	go func() {
		for i := 0; i < writes; i++ {
			if _, err := s.Enqueue(ctx, "otto", "user", "", "msg", 0); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()

	for {
		if _, err := s.DequeueAll(ctx); err != nil {
			t.Fatalf("DequeueAll during concurrent enqueues: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("concurrent Enqueue: %v", err)
			}
			return
		default:
		}
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

// TestDeferHidesUntilDue is the core of deferred delivery: a deferred row must
// be invisible to the drain until its time comes, then reappear.
func TestDeferHidesUntilDue(t *testing.T) {
	st := newInboxStore(t)
	ctx := context.Background()
	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "check my email", 1); err != nil {
		t.Fatal(err)
	}

	got, err := st.DequeueAll(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("first dequeue: %v %d", err, len(got))
	}
	id := got[0].ID
	if got[0].Attempts != 0 {
		t.Errorf("first delivery Attempts = %d, want 0", got[0].Attempts)
	}

	// Defer into the future: not visible.
	requeued, attempts, err := st.Defer(ctx, id, time.Hour)
	if err != nil || !requeued || attempts != 1 {
		t.Fatalf("Defer: requeued=%v attempts=%d err=%v", requeued, attempts, err)
	}
	if got, _ := st.DequeueAll(ctx); len(got) != 0 {
		t.Fatalf("deferred row came back early: %+v", got)
	}

	// Defer into the past: visible again, with the attempt count carried.
	if _, _, err := st.Defer(ctx, id, -time.Second); err != nil {
		t.Fatal(err)
	}
	got, err = st.DequeueAll(ctx)
	if err != nil || len(got) != 1 {
		t.Fatalf("re-dequeue: %v %d", err, len(got))
	}
	if got[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", got[0].Attempts)
	}
	if got[0].Body != "check my email" {
		t.Errorf("body corrupted: %q", got[0].Body)
	}
}

// TestDeferGivesUpAtCap — an Otto that never frees must not spin a row
// forever; the dispatcher needs a definite "stop and tell the user" signal.
func TestDeferGivesUpAtCap(t *testing.T) {
	st := newInboxStore(t)
	ctx := context.Background()
	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "body", 1); err != nil {
		t.Fatal(err)
	}
	got, _ := st.DequeueAll(ctx)
	id := got[0].ID

	for i := 1; i < MaxDeliveryAttempts; i++ {
		requeued, _, err := st.Defer(ctx, id, -time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if !requeued {
			t.Fatalf("gave up early at attempt %d", i)
		}
		if _, err := st.DequeueAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	requeued, attempts, err := st.Defer(ctx, id, -time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if requeued {
		t.Error("should have given up at the cap")
	}
	if attempts != MaxDeliveryAttempts {
		t.Errorf("final attempts = %d, want %d", attempts, MaxDeliveryAttempts)
	}
	// And it stays out of the queue for good.
	if got, _ := st.DequeueAll(ctx); len(got) != 0 {
		t.Errorf("exhausted row returned to the queue: %+v", got)
	}
}

// TestDequeueDoesNotStealDeferredRows guards the subtle failure the
// deliver_after clause in the marking UPDATE exists to prevent: a deferred row
// with a LOWER id than the batch max must not be swept up as delivered, which
// would drop it without ever dispatching it.
func TestDequeueDoesNotStealDeferredRows(t *testing.T) {
	st := newInboxStore(t)
	ctx := context.Background()

	// Row A (low id) will be deferred; row B (high id) arrives later.
	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "deferred one", 1); err != nil {
		t.Fatal(err)
	}
	first, _ := st.DequeueAll(ctx)
	deferredID := first[0].ID
	if _, _, err := st.Defer(ctx, deferredID, time.Hour); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Enqueue(ctx, "otto", "agent", "toto", "later one", 1); err != nil {
		t.Fatal(err)
	}
	batch, err := st.DequeueAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch) != 1 || batch[0].Body != "later one" {
		t.Fatalf("batch = %+v, want only the non-deferred row", batch)
	}

	// The deferred row must still be pending, not silently marked delivered.
	var delivered int
	if err := st.db.QueryRow(`SELECT delivered FROM inbox WHERE id = ?`, deferredID).Scan(&delivered); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Error("deferred row was marked delivered by a later batch — message lost")
	}
}

// TestMigrationAddsColumnsToOldDB — existing installs have a state.db without
// deliver_after/attempts, and CREATE TABLE IF NOT EXISTS will not add them.
func TestMigrationAddsColumnsToOldDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Build a pre-migration inbox by hand, then close.
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE inbox (
		id INTEGER PRIMARY KEY AUTOINCREMENT, ts TEXT NOT NULL, target TEXT NOT NULL,
		source TEXT NOT NULL, sender TEXT NOT NULL, body TEXT NOT NULL,
		delivered INTEGER NOT NULL DEFAULT 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(
		`INSERT INTO inbox(ts,target,source,sender,body) VALUES ('2026-01-01T00:00:00Z','otto','agent','toto','legacy row')`,
	); err != nil {
		t.Fatal(err)
	}
	old.Close()

	// Opening through the store must migrate it in place, without data loss.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a pre-migration DB: %v", err)
	}
	defer st.Close()

	got, err := st.DequeueAll(context.Background())
	if err != nil {
		t.Fatalf("DequeueAll after migration: %v", err)
	}
	if len(got) != 1 || got[0].Body != "legacy row" {
		t.Fatalf("legacy row lost in migration: %+v", got)
	}
	if got[0].Hop != 0 || got[0].Attempts != 0 {
		t.Errorf("migrated defaults wrong: hop=%d attempts=%d", got[0].Hop, got[0].Attempts)
	}
	// And the new machinery works on the migrated row.
	if requeued, _, err := st.Defer(context.Background(), got[0].ID, -time.Second); err != nil || !requeued {
		t.Errorf("Defer on migrated row: requeued=%v err=%v", requeued, err)
	}
}
