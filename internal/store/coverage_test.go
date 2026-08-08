package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Gaps in the store's own coverage, closed deliberately: this is the component
// where a bug is durable. It holds memory, the message bus, and the numbers
// /tokens bills against.

// ─── Usage by model ──────────────────────────────────────────────────────

// UsageByModel is what /tokens costs against a rate card. A model missing from
// the breakdown is a model silently billed as free.
func TestUsageByModel(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	rows := []struct {
		source, model                   string
		in, out, cacheCreate, cacheRead int
	}{
		{"main", "claude-opus-4-8", 100, 20, 5, 50},
		{"main", "claude-opus-4-8", 200, 30, 0, 10},
		{"classify", "claude-haiku-4-5", 10, 1, 0, 0},
	}
	for _, r := range rows {
		if err := s.RecordUsage(ctx, r.source, r.model, r.in, r.out, r.cacheCreate, r.cacheRead); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	byModel, err := s.UsageByModel(ctx)
	if err != nil {
		t.Fatalf("UsageByModel: %v", err)
	}
	if len(byModel) != 2 {
		t.Fatalf("got %d models, want 2", len(byModel))
	}

	found := map[string]ModelTotals{}
	for _, u := range byModel {
		found[u.Model] = u
	}
	opus, ok := found["claude-opus-4-8"]
	if !ok {
		t.Fatal("opus missing from the breakdown")
	}
	if opus.InputTokens != 300 {
		t.Errorf("opus input = %d, want 300 summed across rows", opus.InputTokens)
	}
	if opus.OutputTokens != 50 {
		t.Errorf("opus output = %d, want 50", opus.OutputTokens)
	}
	if opus.CacheRead != 60 {
		t.Errorf("opus cache read = %d, want 60", opus.CacheRead)
	}
	if opus.CacheCreation != 5 {
		t.Errorf("opus cache creation = %d, want 5", opus.CacheCreation)
	}
}

func TestUsageByModelEmpty(t *testing.T) {
	got, err := openTestStore(t).UsageByModel(context.Background())
	if err != nil {
		t.Fatalf("UsageByModel on an empty store: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows, want none", len(got))
	}
}

// ─── Deferred delivery ───────────────────────────────────────────────────

// A message deferred forever would be worse than one dropped: the user watched
// Toto accept it, so the failure has to eventually surface.
func TestDeferExhaustsAttempts(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	id, err := s.Enqueue(ctx, "otto", "agent", "toto", "please do this", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DequeueAll(ctx); err != nil {
		t.Fatal(err)
	}

	// Defer repeatedly with no delay so each is immediately visible again.
	var lastAttempts int
	for i := 0; i < MaxDeliveryAttempts+2; i++ {
		requeued, attempts, err := s.Defer(ctx, id, 0)
		if err != nil {
			t.Fatalf("defer %d: %v", i, err)
		}
		lastAttempts = attempts
		if !requeued {
			if attempts < MaxDeliveryAttempts {
				t.Errorf("gave up after %d attempts, want at least %d", attempts, MaxDeliveryAttempts)
			}
			break
		}
		if _, err := s.DequeueAll(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if lastAttempts < MaxDeliveryAttempts {
		t.Errorf("final attempts = %d, want the cap to have been reached", lastAttempts)
	}

	// Exhausted rows stay delivered so they never come back.
	queued, _, err := s.InboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d rows still queued after exhaustion, want 0", queued)
	}
}

func TestDeferUnknownIDErrors(t *testing.T) {
	if _, _, err := openTestStore(t).Defer(context.Background(), 9999, time.Minute); err == nil {
		t.Error("deferring a nonexistent row should error rather than silently succeed")
	}
}

// The predicate that marks rows delivered must mirror the SELECT exactly. If it
// did not, a deferred row with a smaller id would sit inside the marked range
// and be dropped without ever being dispatched.
func TestDequeueDoesNotSwallowDeferredRows(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	deferredID, err := s.Enqueue(ctx, "otto", "agent", "toto", "first", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DequeueAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Defer(ctx, deferredID, time.Hour); err != nil {
		t.Fatal(err)
	}

	// A newer message arrives and is drained while the older one is still
	// invisible.
	if _, err := s.Enqueue(ctx, "otto", "agent", "toto", "second", 1); err != nil {
		t.Fatal(err)
	}
	got, err := s.DequeueAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != "second" {
		t.Fatalf("dequeued %v, want only the visible message", got)
	}

	queued, ready, err := s.InboxDepth(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if queued != 1 || ready != 0 {
		t.Errorf("depth = %d queued / %d ready, want the deferred row still waiting", queued, ready)
	}
}

func TestEnqueueRejectsBadInput(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	tests := []struct {
		name                         string
		target, source, sender, body string
		hop                          int
		wantSentinel                 error
	}{
		{name: "bad target", target: "nobody", source: "agent", sender: "toto", body: "x"},
		{name: "bad source", target: "otto", source: "ghost", sender: "toto", body: "x"},
		{name: "user row with sender", target: "otto", source: "user", sender: "toto", body: "x"},
		{name: "agent row without sender", target: "otto", source: "agent", sender: "", body: "x"},
		{name: "agent row with bad sender", target: "otto", source: "agent", sender: "gremlin", body: "x"},
		{name: "empty body", target: "otto", source: "user", sender: "", body: "   "},
		{name: "negative hop", target: "otto", source: "user", sender: "", body: "x", hop: -1},
		{name: "over cap", target: "otto", source: "agent", sender: "toto", body: "x", hop: MaxBusHop + 1, wantSentinel: ErrBusHopExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.Enqueue(ctx, tc.target, tc.source, tc.sender, tc.body, tc.hop)
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if tc.wantSentinel != nil && !errors.Is(err, tc.wantSentinel) {
				t.Errorf("error = %v, want it to wrap %v so callers can distinguish it", err, tc.wantSentinel)
			}
		})
	}
}

// ─── Turn channel tagging ────────────────────────────────────────────────

func TestTurnViaRoundTrips(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if _, err := s.AppendTurnVia(ctx, "otto", "user", "spoken question", ViaVoice); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendTurn(ctx, "otto", "user", "typed question"); err != nil {
		t.Fatal(err)
	}

	got, err := s.RecentTurns(ctx, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d turns, want 2", len(got))
	}
	if got[0].Via != ViaVoice {
		t.Errorf("first turn via = %q, want %q", got[0].Via, ViaVoice)
	}
	// AppendTurn must default to text so every pre-existing row stays correct
	// without a backfill.
	if got[1].Via != ViaText {
		t.Errorf("second turn via = %q, want %q", got[1].Via, ViaText)
	}
}

func TestAppendTurnViaDefaultsBlankToText(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.AppendTurnVia(ctx, "otto", "user", "hi", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecentTurns(ctx, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Via != ViaText {
		t.Errorf("via = %q, want it defaulted to %q", got[0].Via, ViaText)
	}
}

func TestSearchCarriesVia(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	if _, err := s.AppendTurnVia(ctx, "otto", "user", "deployment target fly", ViaVoice); err != nil {
		t.Fatal(err)
	}
	got, err := s.SearchFTS(ctx, "deployment", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d hits, want 1", len(got))
	}
	if got[0].Via != ViaVoice {
		t.Errorf("search result via = %q, want it preserved so a reader can tell", got[0].Via)
	}
}

// ─── Vectors ─────────────────────────────────────────────────────────────

func TestPutVectorReplacesOnConflict(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	id, err := s.AppendTurn(ctx, "otto", "user", "embed me")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutVector(ctx, id, "modelA", []float32{1, 0, 0}); err != nil {
		t.Fatal(err)
	}
	// Re-embedding the same turn (a model swap) must replace, not duplicate:
	// two vectors for one turn would let it appear twice in a ranked result.
	if err := s.PutVector(ctx, id, "modelB", []float32{0, 1, 0}); err != nil {
		t.Fatalf("re-embedding failed: %v", err)
	}

	got, err := s.SearchSemanticModel(ctx, []float32{0, 1, 0}, "modelB", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("got %d hits, want exactly 1 — a replaced vector must not duplicate the turn", len(got))
	}
}
