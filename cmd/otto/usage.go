//go:build unix

package main

import (
	"context"
	"log"

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
