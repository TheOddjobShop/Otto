//go:build unix

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"otto/internal/store"
)

// Activity logging: what Otto is DOING, as opposed to what he is SAYING.
//
// During a long agentic turn the assistant-text stream goes quiet for minutes
// while tools run. That silence is precisely when the user sends "what's going
// on?" and Toto has to answer. Before this, the only material Toto had was a
// 600-byte tail of Otto's prose, which during a tool sequence is stale or
// empty. Now every tool call and result is recorded, both in a bounded
// in-memory ring (read synchronously by the Toto dispatch path) and in the
// `activity` table (durable, survives restart, prunable).
//
// The two live side by side on purpose: the ring answers "right now" with no
// DB round-trip on Toto's latency-sensitive path, and the table answers
// "earlier" and outlives the process.

// activityRingCap bounds the per-turn in-memory ring. Ten lines is enough for
// Toto to characterize what Otto is doing ("he's been running tests and
// editing the auth file") without pushing a Haiku prompt past the point where
// the persona instructions start losing weight.
const activityRingCap = 10

// activityDetailCap bounds one rendered detail line before it reaches either
// the ring or the store. Mirrors the store's own cap; applied here too so the
// in-memory copy can't hold a megabyte of tool argument.
const activityDetailCap = 200

// turnKeySeq makes turn keys unique within a process. Combined with the
// process start time in newTurnKey, keys stay unique ACROSS restarts too —
// without that, a fresh process starting its counter at 1 would collide with
// turn 1 of a previous boot and mix two unrelated turns into one query.
var turnKeySeq atomic.Uint64

// processStartNano is sampled once so every turn key of this process shares a
// boot-unique prefix.
var processStartNano = time.Now().UnixNano()

// newTurnKey returns a process-unique, restart-safe key identifying one turn.
func newTurnKey() string {
	return fmt.Sprintf("%d-%d", processStartNano, turnKeySeq.Add(1))
}

// activityEntry is the in-memory form of one activity line.
type activityEntry struct {
	At      time.Time
	Kind    string
	Tool    string
	Detail  string
	IsError bool
}

// summarizeToolInput renders a tool's raw JSON arguments as one short line.
//
// Tools are summarized by the argument that actually identifies the work:
// which command, which file, which pattern. A generic dump of the whole input
// object would be both longer and less legible — "Bash {\"command\":\"npm
// test\",\"description\":\"…\"}" tells the reader less than "Bash npm test".
func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil {
		return ""
	}

	// Preferred argument per tool family, most specific first. MCP tools
	// (mcp__server__tool) fall through to the generic path below.
	preferred := map[string][]string{
		"Bash":         {"command"},
		"Read":         {"file_path"},
		"Write":        {"file_path"},
		"Edit":         {"file_path"},
		"NotebookEdit": {"notebook_path"},
		"Glob":         {"pattern"},
		"Grep":         {"pattern"},
		"WebFetch":     {"url"},
		"WebSearch":    {"query"},
		"Task":         {"description"},
	}
	if keys, ok := preferred[name]; ok {
		for _, k := range keys {
			if s, ok := args[k].(string); ok && s != "" {
				return clip(oneLine(s), activityDetailCap)
			}
		}
	}

	// Generic fallback: the first string-valued argument by sorted key name, so
	// the choice is deterministic rather than dependent on map iteration order.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if s, ok := args[k].(string); ok && s != "" {
			return clip(k+"="+oneLine(s), activityDetailCap)
		}
	}
	return ""
}

// oneLine collapses whitespace so a multi-line command renders as one entry.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// clip truncates to n runes with an ellipsis, never splitting a rune.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// logActivity persists one activity row. Best-effort: a nil store or a write
// error is logged and swallowed, never surfaced, because an activity-log
// failure must not affect the turn it is describing.
func logActivity(ctx context.Context, s *store.Store, e store.ActivityEntry) {
	if s == nil {
		return
	}
	if err := s.AppendActivity(ctx, e); err != nil {
		log.Printf("activity log (%s/%s): %v", e.Kind, e.Tool, err)
	}
}

// formatActivityForPet renders activity lines for injection into a pet's
// per-call prompt. Returns "" when there is nothing to show, so the caller can
// omit the block entirely rather than claiming Otto is doing nothing.
//
// Times are rendered as clock times rather than deltas: Toto is answering
// "what's he doing", and a wall-clock sequence reads more naturally than
// "-14s" when he paraphrases it.
func formatActivityForPet(entries []activityEntry) string {
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("WHAT OTTO IS ACTUALLY DOING (his most recent actions, newest last):\n")
	for _, e := range entries {
		stamp := e.At.Format("15:04:05")
		switch e.Kind {
		case store.ActivityTool:
			fmt.Fprintf(&b, "  %s  %-10s %s\n", stamp, e.Tool, e.Detail)
		case store.ActivityResult:
			if e.IsError {
				fmt.Fprintf(&b, "  %s  %-10s failed: %s\n", stamp, e.Tool, e.Detail)
			}
			// Successful results are omitted: they double the line count and
			// add nothing a reader can act on. Failures stay because "his
			// tests are failing" is exactly the useful thing to relay.
		case store.ActivityTurnStart:
			fmt.Fprintf(&b, "  %s  started on: %s\n", stamp, e.Detail)
		}
	}
	b.WriteString("\nThis is a factual log of tool calls, not something Otto said. ")
	b.WriteString("Use it to describe what he's up to in your own voice — ")
	b.WriteString("\"he's rerunning your tests\" — and never read it out line by line.")
	return b.String()
}
