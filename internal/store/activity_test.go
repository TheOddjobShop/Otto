package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

func appendN(t *testing.T, s *Store, turnKey string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		if err := s.AppendActivity(ctx, ActivityEntry{
			Persona: "otto", TurnKey: turnKey, Kind: ActivityTool,
			Tool: "Bash", Detail: "go test ./...",
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

func TestAppendAndReadActivity(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	entries := []ActivityEntry{
		{Persona: "otto", TurnKey: "t1", Kind: ActivityTurnStart, Detail: "run the tests"},
		{Persona: "otto", TurnKey: "t1", Kind: ActivityTool, Tool: "Bash", Detail: "go test ./..."},
		{Persona: "otto", TurnKey: "t1", Kind: ActivityResult, Tool: "Bash", Detail: "exit 1", IsError: true},
		{Persona: "otto", TurnKey: "t1", Kind: ActivityTurnEnd, Detail: "ok in 4s"},
	}
	for _, e := range entries {
		if err := s.AppendActivity(ctx, e); err != nil {
			t.Fatalf("AppendActivity: %v", err)
		}
	}

	got, err := s.ActivityForTurn(ctx, "t1", 10)
	if err != nil {
		t.Fatalf("ActivityForTurn: %v", err)
	}
	if len(got) != len(entries) {
		t.Fatalf("got %d rows, want %d", len(got), len(entries))
	}
	// Chronological order is the contract — a reader should see the sequence as
	// it happened, not reversed.
	for i := range entries {
		if got[i].Kind != entries[i].Kind {
			t.Errorf("row %d kind = %q, want %q (order is wrong)", i, got[i].Kind, entries[i].Kind)
		}
	}
	if !got[2].IsError {
		t.Error("the failing result should round-trip its error flag")
	}
	if got[1].Tool != "Bash" {
		t.Errorf("tool = %q, want Bash", got[1].Tool)
	}
	if got[0].TS.IsZero() || time.Since(got[0].TS) > time.Minute {
		t.Errorf("timestamp = %v, want roughly now", got[0].TS)
	}
}

// Grouping by turn is the whole point: a bus turn and a Telegram turn interleave
// in the table, and reporting one turn's tools as another's would be exactly the
// stale answer the activity log exists to prevent.
func TestActivityForTurnIsolatesTurns(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	appendN(t, s, "turn-a", 3)
	appendN(t, s, "turn-b", 2)

	a, err := s.ActivityForTurn(ctx, "turn-a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 3 {
		t.Errorf("turn-a has %d rows, want 3", len(a))
	}
	for _, e := range a {
		if e.TurnKey != "turn-a" {
			t.Errorf("row leaked from %q into turn-a's results", e.TurnKey)
		}
	}
}

func TestActivityForTurnLimitTakesNewest(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	for i, detail := range []string{"first", "second", "third", "fourth"} {
		if err := s.AppendActivity(ctx, ActivityEntry{
			Persona: "otto", TurnKey: "t1", Kind: ActivityTool, Tool: "Read", Detail: detail,
		}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	got, err := s.ActivityForTurn(ctx, "t1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	// Newest two, still in chronological order.
	if got[0].Detail != "third" || got[1].Detail != "fourth" {
		t.Errorf("got %q/%q, want the two most recent in order", got[0].Detail, got[1].Detail)
	}
}

func TestActivityForTurnUnknownKeyIsEmpty(t *testing.T) {
	got, err := openTestStore(t).ActivityForTurn(context.Background(), "nope", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d rows for an unknown turn, want none", len(got))
	}
}

func TestActivityZeroLimitReturnsNothing(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appendN(t, s, "t1", 3)

	if got, err := s.ActivityForTurn(ctx, "t1", 0); err != nil || len(got) != 0 {
		t.Errorf("ActivityForTurn(limit=0) = (%d rows, %v), want none", len(got), err)
	}
	if got, err := s.RecentActivity(ctx, 0); err != nil || len(got) != 0 {
		t.Errorf("RecentActivity(limit=0) = (%d rows, %v), want none", len(got), err)
	}
}

func TestRecentActivitySpansTurns(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appendN(t, s, "turn-a", 2)
	appendN(t, s, "turn-b", 2)

	got, err := s.RecentActivity(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d rows, want all 4 across both turns", len(got))
	}
	if got[0].TurnKey != "turn-a" || got[3].TurnKey != "turn-b" {
		t.Errorf("ordering is wrong: first=%q last=%q", got[0].TurnKey, got[3].TurnKey)
	}
}

// A tool echoing a large argument must not be able to bloat the table or the
// Haiku prompt these rows feed.
func TestAppendActivityCapsDetail(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.AppendActivity(ctx, ActivityEntry{
		Persona: "otto", TurnKey: "t1", Kind: ActivityTool, Tool: "Bash",
		Detail: strings.Repeat("x", activityDetailCap*3),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ActivityForTurn(ctx, "t1", 1)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got[0].Detail)); n > activityDetailCap+1 {
		t.Errorf("stored detail is %d runes, want it capped near %d", n, activityDetailCap)
	}
	if !strings.HasSuffix(got[0].Detail, "…") {
		t.Error("a truncated detail should be marked with an ellipsis rather than silently cut")
	}
}

// Multi-byte details must not be split mid-rune, which would store invalid
// UTF-8 and corrupt the prompt these rows are injected into.
func TestAppendActivityCapIsRuneSafe(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.AppendActivity(ctx, ActivityEntry{
		Persona: "otto", TurnKey: "t1", Kind: ActivityTool, Tool: "Read",
		Detail: strings.Repeat("日", activityDetailCap*2),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ActivityForTurn(ctx, "t1", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got[0].Detail {
		if r == '�' {
			t.Fatal("detail contains a replacement character — a rune was split")
		}
	}
}

func TestPruneActivityKeepsNewest(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appendN(t, s, "t1", 20)

	removed, err := s.PruneActivity(ctx, 5)
	if err != nil {
		t.Fatalf("PruneActivity: %v", err)
	}
	if removed != 15 {
		t.Errorf("removed %d rows, want 15", removed)
	}
	got, err := s.RecentActivity(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Errorf("%d rows remain, want 5", len(got))
	}
}

func TestPruneActivityNoOpCases(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	appendN(t, s, "t1", 3)

	if n, err := s.PruneActivity(ctx, 0); err != nil || n != 0 {
		t.Errorf("keep=0 removed %d rows (err %v), want a no-op", n, err)
	}
	// Fewer rows than the cap: nothing to remove.
	if n, err := s.PruneActivity(ctx, 100); err != nil || n != 0 {
		t.Errorf("keep>rows removed %d (err %v), want 0", n, err)
	}
	if got, _ := s.RecentActivity(ctx, 10); len(got) != 3 {
		t.Errorf("%d rows remain, want all 3 untouched", len(got))
	}
}
