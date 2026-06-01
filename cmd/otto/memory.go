//go:build unix

package main

import (
	"context"
	"fmt"
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

// currentTimeBlock formats a structured block describing the moment t, in both
// the host's local timezone and UTC. The helper is deterministic — callers pass
// the instant explicitly so tests can pin it. Use currentTimeBlockNow() in
// production code paths.
func currentTimeBlock(t time.Time) string {
	local := t.In(time.Local)
	utc := t.UTC()

	zoneName, offsetSeconds := local.Zone()
	sign := "+"
	if offsetSeconds < 0 {
		sign = "-"
		offsetSeconds = -offsetSeconds
	}
	offH := offsetSeconds / 3600
	offM := (offsetSeconds % 3600) / 60
	offset := fmt.Sprintf("UTC%s%02d:%02d", sign, offH, offM)

	var b strings.Builder
	b.WriteString("───────────────────────────────────────────────\n")
	b.WriteString("  CURRENT TIME (sampled at this turn)\n")
	b.WriteString("───────────────────────────────────────────────\n\n")
	b.WriteString(fmt.Sprintf("Local:   %s %s (%s)\n",
		local.Format("Mon 2006-01-02 15:04:05"), zoneName, offset))
	b.WriteString(fmt.Sprintf("UTC:     %s", utc.Format("2006-01-02 15:04:05")))
	return b.String()
}

// currentTimeBlockNow is the production wrapper around currentTimeBlock —
// it samples time.Now() at call time so each composed prompt reflects the
// actual moment of composition rather than process boot.
func currentTimeBlockNow() string {
	return currentTimeBlock(time.Now())
}

// composePromptWithTimeAndMemory layers the current-time block and the memory
// core onto a base persona prompt, in that order. The time block is sampled at
// call time (via currentTimeBlockNow) so each turn reflects the exact moment
// the prompt is composed. Memory is appended last via composeMemoryPrompt so
// its responsibility stays narrow.
func composePromptWithTimeAndMemory(base string, core *memory.Core) string {
	timeBlock := currentTimeBlockNow()
	var withTime string
	switch {
	case base == "":
		withTime = timeBlock
	default:
		withTime = base + "\n\n" + timeBlock
	}
	return composeMemoryPrompt(withTime, core)
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
//
// The embed goroutine is intentionally NOT tracked for shutdown: blocking
// SIGTERM on in-flight embeds (up to 30s each) would delay restarts. On
// shutdown a goroutine may race memStore.Close() — the resulting "database is
// closed" error is logged and harmless (WAL keeps the DB consistent; at worst
// one turn's vector is lost and that turn is still keyword-searchable).
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
