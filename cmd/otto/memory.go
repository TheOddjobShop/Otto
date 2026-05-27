//go:build unix

package main

import (
	"context"
	"log"
	"strings"

	"otto/internal/memory"
	"otto/internal/store"
)

// Memory core character caps (rough token proxies). Mirror the values in
// cmd/otto-memory; they only bound writes (which happen via the MCP server),
// so for Otto's read-only Inject they are immaterial but kept consistent.
const (
	memCapChars  = 2200
	userCapChars = 1375
)

// composeMemoryPrompt appends the curated memory core to a base system prompt.
// Returns base unchanged when core is nil or empty. The injected block carries
// its own header (see memory.Core.Inject).
func composeMemoryPrompt(base string, core *memory.Core) string {
	if core == nil {
		return base
	}
	block, err := core.Inject()
	if err != nil {
		log.Printf("memory inject: %v", err)
		return base
	}
	if block == "" {
		return base
	}
	if base == "" {
		return block
	}
	return base + "\n\n" + block
}

// logTurn appends one conversation turn to the store, best-effort. A nil store
// or blank content is a no-op. Errors are logged, never propagated — turn
// logging must never break a reply.
func logTurn(ctx context.Context, st *store.Store, persona, role, content string) {
	if st == nil || strings.TrimSpace(content) == "" {
		return
	}
	if _, err := st.AppendTurn(ctx, persona, role, content); err != nil {
		log.Printf("turn log (%s/%s): %v", persona, role, err)
	}
}
