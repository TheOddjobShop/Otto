//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"otto/internal/claude"
	"otto/internal/store"
)

// recordUsage writes one token_usage row for an observed result event. It is
// best-effort: a nil store (tests / store-disabled configs) or a write error
// is logged and swallowed so a usage failure can never break a reply.
func recordUsage(ctx context.Context, s *store.Store, source, model string, r claude.ResultEvent) {
	if s == nil {
		return
	}
	if model == "" {
		model = "default"
	}
	if err := s.RecordUsage(ctx, source, model, r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens); err != nil {
		log.Printf("usage record (%s): %v", source, err)
	}
}

// formatUsage renders the /tokens reply: a grand total followed by a
// per-source breakdown (input + output only; cache columns stay in the DB).
func formatUsage(totals store.Totals, bySrc []store.SourceTotals) string {
	if len(bySrc) == 0 {
		return "📊 No token usage recorded yet."
	}
	var b strings.Builder
	b.WriteString("📊 Token usage (all-time)\n")
	fmt.Fprintf(&b, "Total: %s in · %s out\n\n",
		thousands(totals.InputTokens), thousands(totals.OutputTokens))
	for _, s := range bySrc {
		fmt.Fprintf(&b, "%-9s %s in · %s out\n",
			s.Source, thousands(s.InputTokens), thousands(s.OutputTokens))
	}
	return strings.TrimRight(b.String(), "\n")
}

// thousands renders n with comma separators (e.g. 1284302 -> "1,284,302").
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
