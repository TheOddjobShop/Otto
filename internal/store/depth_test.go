package store

import (
	"context"
	"testing"
	"time"
)

func TestInboxDepthEmpty(t *testing.T) {
	s := openTestStore(t)
	queued, ready, err := s.InboxDepth(context.Background())
	if err != nil {
		t.Fatalf("InboxDepth: %v", err)
	}
	if queued != 0 || ready != 0 {
		t.Errorf("got queued=%d ready=%d, want 0/0 on an empty inbox", queued, ready)
	}
}

func TestInboxDepthCountsOnlyUndelivered(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for i := 0; i < 3; i++ {
		if _, err := s.Enqueue(ctx, "otto", "agent", "toto", "hi", 1); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if queued, ready, err := s.InboxDepth(ctx); err != nil || queued != 3 || ready != 3 {
		t.Fatalf("got queued=%d ready=%d err=%v, want 3/3", queued, ready, err)
	}

	// Draining marks them delivered, which must take them out of the count —
	// otherwise /status would report a permanently growing backlog.
	if _, err := s.DequeueAll(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if queued, ready, err := s.InboxDepth(ctx); err != nil || queued != 0 || ready != 0 {
		t.Fatalf("after drain got queued=%d ready=%d err=%v, want 0/0", queued, ready, err)
	}
}

// The queued/ready split exists precisely for deferred messages: a hand-off
// waiting on a busy Otto is still queued but not yet ready.
func TestInboxDepthSeparatesDeferredFromReady(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	id, err := s.Enqueue(ctx, "otto", "agent", "toto", "deferred one", 1)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := s.Enqueue(ctx, "otto", "agent", "toto", "ready one", 1); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// Dequeue both (marking delivered), then defer the first back into the
	// future — the exact shape the busy-Otto path produces.
	if _, err := s.DequeueAll(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	requeued, _, err := s.Defer(ctx, id, time.Hour)
	if err != nil {
		t.Fatalf("defer: %v", err)
	}
	if !requeued {
		t.Fatal("expected the message to be requeued")
	}

	queued, ready, err := s.InboxDepth(ctx)
	if err != nil {
		t.Fatalf("InboxDepth: %v", err)
	}
	if queued != 1 {
		t.Errorf("got queued=%d, want 1 (the deferred message is still waiting)", queued)
	}
	if ready != 0 {
		t.Errorf("got ready=%d, want 0 (its retry time has not elapsed)", ready)
	}
}
