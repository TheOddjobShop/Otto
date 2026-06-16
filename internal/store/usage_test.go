package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordUsageAndAggregate(t *testing.T) {
	s := openTestStore(t)

	ctx := context.Background()
	rows := []struct {
		source          string
		model           string
		in, out, cc, cr int
	}{
		{"main", "claude-opus-4-8", 100, 20, 5, 1000},
		{"main", "claude-sonnet-4-6", 50, 10, 0, 500},
		{"toto", "claude-haiku-4-5", 30, 5, 0, 200},
	}
	for _, r := range rows {
		if err := s.RecordUsage(ctx, r.source, r.model, r.in, r.out, r.cc, r.cr); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	totals, err := s.UsageTotals(ctx)
	if err != nil {
		t.Fatalf("UsageTotals: %v", err)
	}
	if totals.InputTokens != 180 || totals.OutputTokens != 35 {
		t.Errorf("totals = %+v, want input 180 output 35", totals)
	}
	if totals.CacheCreation != 5 || totals.CacheRead != 1700 {
		t.Errorf("totals cache = %+v, want cc 5 cr 1700", totals)
	}

	bySrc, err := s.UsageBySource(ctx)
	if err != nil {
		t.Fatalf("UsageBySource: %v", err)
	}
	if len(bySrc) != 2 {
		t.Fatalf("got %d sources, want 2", len(bySrc))
	}
	if bySrc[0].Source != "main" || bySrc[0].InputTokens != 150 || bySrc[0].OutputTokens != 30 {
		t.Errorf("bySrc[0] = %+v, want main 150/30", bySrc[0])
	}
	if bySrc[1].Source != "toto" || bySrc[1].InputTokens != 30 {
		t.Errorf("bySrc[1] = %+v, want toto 30", bySrc[1])
	}
}

func TestUsageTotalsEmpty(t *testing.T) {
	s := openTestStore(t)
	totals, err := s.UsageTotals(context.Background())
	if err != nil {
		t.Fatalf("UsageTotals: %v", err)
	}
	if totals.InputTokens != 0 || totals.OutputTokens != 0 {
		t.Errorf("empty totals = %+v, want all zero", totals)
	}
}
