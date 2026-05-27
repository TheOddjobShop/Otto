//go:build unix

package main

import (
	"context"
	"log"
	"strings"
	"time"

	"otto/internal/embed"
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

// embedAndStore embeds content and persists the vector for turnID, best-effort.
// Synchronous and 30s-bounded; callers run it in a goroutine off the reply
// path. Errors are logged, never propagated.
func embedAndStore(st *store.Store, emb embed.Embedder, turnID int64, content string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := emb.Embed(ctx, content)
	if err != nil {
		log.Printf("embed turn %d: %v", turnID, err)
		return
	}
	if err := st.PutVector(ctx, turnID, r.Model, r.Vector); err != nil {
		log.Printf("put vector %d: %v", turnID, err)
	}
}

// logTurn appends one conversation turn (best-effort) and, when emb is non-nil,
// asynchronously embeds it for semantic search. A nil store or blank content is
// a no-op. Turn logging must never break a reply, so embedding runs in a
// detached goroutine.
func logTurn(ctx context.Context, st *store.Store, emb embed.Embedder, persona, role, content string) {
	if st == nil || strings.TrimSpace(content) == "" {
		return
	}
	id, err := st.AppendTurn(ctx, persona, role, content)
	if err != nil {
		log.Printf("turn log (%s/%s): %v", persona, role, err)
		return
	}
	if emb != nil {
		go embedAndStore(st, emb, id, content)
	}
}
