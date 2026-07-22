//go:build unix

package main

import (
	"fmt"
	"sort"
	"strings"

	"otto/internal/store"
)

// Dollar-cost estimation for /tokens. The token-tracker spec listed this as a
// non-goal ("per-model pricing — explicitly out of scope for now"), but every
// input it needs is already persisted: token_usage carries source, model, and
// all four token classes per turn.
//
// Rates are published list prices in USD per million tokens. They are a
// snapshot, not a live feed — Otto has no billing API access, so this is an
// ESTIMATE. Anything not in this table is reported as unpriced rather than
// silently costed at zero: a total that quietly omits a model is worse than a
// total that says what it left out.

// modelPricing is the four-rate card for one model, USD per million tokens.
//
// The four token classes bill differently and do NOT overlap: the API reports
// input_tokens as the uncached remainder only, so
//
//	prompt size = input + cache_creation + cache_read
//
// and summing all four classes at their own rates double-counts nothing.
type modelPricing struct {
	input      float64 // uncached prompt tokens
	output     float64 // generated tokens
	cacheWrite float64 // tokens written to the prompt cache
	cacheRead  float64 // tokens served from the prompt cache
}

// cacheWriteMultiplier is the cache-write premium over the base input rate for
// the DEFAULT 5-minute cache TTL. A 1-hour TTL is billed at 2× instead.
//
// Otto cannot observe which TTL its `claude` subprocesses used — the stream
// reports token counts, not cache TTLs — so the estimate assumes the 5-minute
// default. If Claude Code is caching at 1h, real cache-write spend is higher
// than reported, which is why /tokens labels the figure an estimate.
const cacheWriteMultiplier = 1.25

// cacheReadMultiplier is the cache-read discount off the base input rate.
const cacheReadMultiplier = 0.1

// priced builds a rate card from the base input/output rates, deriving the two
// cache rates from them. Keeps the table below to the two numbers that are
// actually published per model.
func priced(input, output float64) modelPricing {
	return modelPricing{
		input:      input,
		output:     output,
		cacheWrite: input * cacheWriteMultiplier,
		cacheRead:  input * cacheReadMultiplier,
	}
}

// modelPrices maps model id → rate card. Keys are the exact ids Otto records
// in token_usage (see classify.go's model constants and the pet models).
//
// Deliberately NOT included: "default" — the label recordUsage stores when a
// turn inherited Claude Code's own model choice. Otto doesn't know which model
// that was, so pricing it would be a guess.
var modelPrices = map[string]modelPricing{
	ottoCodingModel:  priced(5.00, 25.00), // claude-opus-4-8
	ottoDefaultModel: priced(3.00, 15.00), // claude-sonnet-4-6
	classifierModel:  priced(1.00, 5.00),  // claude-haiku-4-5 (also totoModel / tootModel)
}

// costOf returns the estimated USD cost of one model's aggregate usage, and
// whether that model has a rate card at all.
func costOf(m store.ModelTotals) (float64, bool) {
	p, ok := modelPrices[m.Model]
	if !ok {
		return 0, false
	}
	const perMillion = 1_000_000.0
	cost := float64(m.InputTokens)/perMillion*p.input +
		float64(m.OutputTokens)/perMillion*p.output +
		float64(m.CacheCreation)/perMillion*p.cacheWrite +
		float64(m.CacheRead)/perMillion*p.cacheRead
	return cost, true
}

// costBreakdown is the priced view of all recorded usage.
type costBreakdown struct {
	total      float64            // summed cost of every priced model
	perModel   map[string]float64 // model id → cost, priced models only
	unpriced   []string           // model ids with recorded tokens but no rate card
	unpricedTk int                // total tokens attributed to those models
}

// estimateCost prices per-model usage. Models without a rate card are
// collected in unpriced rather than being dropped or zero-costed, so the
// caller can say plainly what the total excludes.
func estimateCost(byModel []store.ModelTotals) costBreakdown {
	cb := costBreakdown{perModel: map[string]float64{}}
	for _, m := range byModel {
		cost, ok := costOf(m)
		if !ok {
			tokens := m.InputTokens + m.OutputTokens + m.CacheCreation + m.CacheRead
			if tokens == 0 {
				continue // nothing recorded; not worth naming
			}
			cb.unpriced = append(cb.unpriced, m.Model)
			cb.unpricedTk += tokens
			continue
		}
		cb.total += cost
		cb.perModel[m.Model] += cost
	}
	sort.Strings(cb.unpriced)
	return cb
}

// usd renders a dollar amount at a precision that stays informative across
// four orders of magnitude: sub-cent spend would otherwise render as "$0.00"
// and read as "free" on a fresh install.
func usd(v float64) string {
	switch {
	case v == 0:
		return "$0.00"
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	case v < 1:
		return fmt.Sprintf("$%.3f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}

// formatCost renders the cost section of the /tokens reply. Returns "" when
// nothing has been recorded, so the caller can omit the section entirely.
func formatCost(cb costBreakdown) string {
	if len(cb.perModel) == 0 && len(cb.unpriced) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Est. cost: %s\n", usd(cb.total))

	// Per-model lines, most expensive first — that's the one worth acting on.
	models := make([]string, 0, len(cb.perModel))
	for m := range cb.perModel {
		models = append(models, m)
	}
	sort.Slice(models, func(i, j int) bool {
		if cb.perModel[models[i]] != cb.perModel[models[j]] {
			return cb.perModel[models[i]] > cb.perModel[models[j]]
		}
		return models[i] < models[j] // stable tiebreak
	})
	for _, m := range models {
		fmt.Fprintf(&b, "  %-18s %s\n", shortModel(m), usd(cb.perModel[m]))
	}
	if len(cb.unpriced) > 0 {
		fmt.Fprintf(&b, "  (excludes %s tokens on: %s)\n",
			thousands(cb.unpricedTk), strings.Join(cb.unpriced, ", "))
	}
	return b.String()
}

// shortModel trims the "claude-" prefix for display. The full id stays in the
// DB; this is only to keep the /tokens reply readable on a phone.
func shortModel(m string) string { return strings.TrimPrefix(m, "claude-") }
