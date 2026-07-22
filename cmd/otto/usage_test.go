//go:build unix

package main

import (
	"strings"
	"testing"

	"otto/internal/store"
)

func TestFormatUsage(t *testing.T) {
	totals := store.Totals{InputTokens: 1284302, OutputTokens: 96540}
	bySrc := []store.SourceTotals{
		{Source: "main", Totals: store.Totals{InputTokens: 980120, OutputTokens: 71200}},
		{Source: "classify", Totals: store.Totals{InputTokens: 26482, OutputTokens: 1200}},
	}
	byModel := []store.ModelTotals{
		{Model: ottoDefaultModel, Totals: store.Totals{InputTokens: 980120, OutputTokens: 71200}},
		{Model: classifierModel, Totals: store.Totals{InputTokens: 26482, OutputTokens: 1200}},
	}
	got := formatUsage(totals, bySrc, byModel)

	for _, want := range []string{
		"1,284,302 in", "96,540 out", "main", "classify", "980,120 in",
		"Est. cost:", "sonnet-4-6", "estimate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("formatUsage output missing %q\n---\n%s", want, got)
		}
	}
}

func TestFormatUsageEmpty(t *testing.T) {
	got := formatUsage(store.Totals{}, nil, nil)
	if !strings.Contains(got, "No token usage recorded yet") {
		t.Errorf("empty output = %q", got)
	}
}
