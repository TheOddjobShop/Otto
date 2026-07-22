//go:build unix

package main

import (
	"strings"
	"testing"

	"otto/internal/store"
)

// TestCostOfKnownModel pins the arithmetic against a hand-computed figure so a
// future rate edit or a unit slip (per-thousand vs per-million) fails loudly.
func TestCostOfKnownModel(t *testing.T) {
	// 1M input, 1M output, 1M cache-write, 1M cache-read on Opus 4.8:
	//   input      1M × $5.00  = 5.00
	//   output     1M × $25.00 = 25.00
	//   cacheWrite 1M × $6.25  = 6.25   (5.00 × 1.25)
	//   cacheRead  1M × $0.50  = 0.50   (5.00 × 0.10)
	//                            ------
	//                            36.75
	got, ok := costOf(store.ModelTotals{
		Model: ottoCodingModel,
		Totals: store.Totals{
			InputTokens:   1_000_000,
			OutputTokens:  1_000_000,
			CacheCreation: 1_000_000,
			CacheRead:     1_000_000,
		},
	})
	if !ok {
		t.Fatalf("%s should be priced", ottoCodingModel)
	}
	if want := 36.75; got != want {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// TestAllRoutedModelsArePriced guards the coupling between classify.go's model
// constants and the rate table: if a new tier is introduced for routing but not
// priced, /tokens would silently under-report until someone noticed.
func TestAllRoutedModelsArePriced(t *testing.T) {
	for _, m := range []string{ottoCodingModel, ottoDefaultModel, classifierModel, totoModel, tootModel} {
		if _, ok := modelPrices[m]; !ok {
			t.Errorf("model %q is used at runtime but has no rate card", m)
		}
	}
}

// TestUnpricedModelsAreReportedNotDropped is the honesty property: a model with
// no rate card must be named in the output, never silently costed at zero.
func TestUnpricedModelsAreReportedNotDropped(t *testing.T) {
	cb := estimateCost([]store.ModelTotals{
		{Model: ottoDefaultModel, Totals: store.Totals{InputTokens: 1_000_000}},
		{Model: "default", Totals: store.Totals{InputTokens: 500_000, OutputTokens: 250_000}},
	})

	if want := 3.00; cb.total != want {
		t.Errorf("total = %v, want %v (only the priced model)", cb.total, want)
	}
	if len(cb.unpriced) != 1 || cb.unpriced[0] != "default" {
		t.Fatalf("unpriced = %v, want [default]", cb.unpriced)
	}
	if want := 750_000; cb.unpricedTk != want {
		t.Errorf("unpricedTk = %d, want %d", cb.unpricedTk, want)
	}

	out := formatCost(cb)
	if !strings.Contains(out, "excludes 750,000 tokens on: default") {
		t.Errorf("formatCost must name what it excluded, got:\n%s", out)
	}
}

// TestZeroTokenModelsAreNotNamed keeps the exclusion note meaningful — a model
// row with no tokens isn't an omission worth reporting.
func TestZeroTokenModelsAreNotNamed(t *testing.T) {
	cb := estimateCost([]store.ModelTotals{{Model: "some-unknown-model"}})
	if len(cb.unpriced) != 0 {
		t.Errorf("zero-token model should not be listed as unpriced: %v", cb.unpriced)
	}
	if got := formatCost(cb); got != "" {
		t.Errorf("formatCost on empty usage = %q, want empty", got)
	}
}

// TestFormatCostOrdersByCostDescending — the expensive model is the actionable
// one, so it leads.
func TestFormatCostOrdersByCostDescending(t *testing.T) {
	out := formatCost(estimateCost([]store.ModelTotals{
		{Model: classifierModel, Totals: store.Totals{InputTokens: 1_000_000}},  // $1.00
		{Model: ottoCodingModel, Totals: store.Totals{InputTokens: 1_000_000}},  // $5.00
		{Model: ottoDefaultModel, Totals: store.Totals{InputTokens: 1_000_000}}, // $3.00
	}))
	opus := strings.Index(out, shortModel(ottoCodingModel))
	sonnet := strings.Index(out, shortModel(ottoDefaultModel))
	haiku := strings.Index(out, shortModel(classifierModel))
	if !(opus < sonnet && sonnet < haiku) {
		t.Errorf("models not ordered by descending cost:\n%s", out)
	}
}

// TestUSDPrecision — sub-cent spend must not render as "$0.00", which reads as
// "free" on a fresh install rather than "very cheap".
func TestUSDPrecision(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "$0.00"},
		{0.0003, "$0.0003"},
		{0.25, "$0.250"},
		{12.5, "$12.50"},
	}
	for _, c := range cases {
		if got := usd(c.in); got != c.want {
			t.Errorf("usd(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
