//go:build unix

package main

import (
	"context"
	"log"
	"strings"
	"time"

	"otto/internal/claude"
)

// Rotation flush: before the rotator clears a session, give Claude one cheap
// pass over that session to write anything durable into the curated memory
// core.
//
// The 2026-05-27 rotation plan listed this as a v1 non-goal, on the reasoning
// that inline memory_add during the conversation already persists facts. That
// holds only when Otto (or the user) thought to save something mid-conversation.
// When neither did, a rotation silently drops everything that wasn't already
// in USER.md / MEMORY.md — the session is the only copy of "the user switched
// their deploy target to fly.io", and after the clear it's gone from context.
// session_search can still find the turn, but only if Otto later thinks to look.
//
// The flush is deliberately narrow: one Haiku turn, memory_add as the only
// permitted tool, a hard timeout, and no user-visible output.

const (
	// flushModel pins the distillation pass to the cheap tier. Extraction from
	// a transcript already in context is not a reasoning-heavy task, and this
	// runs on every rotation — the cost has to stay near-zero or the flush is
	// worse than the problem it solves.
	flushModel = "claude-haiku-4-5"

	// flushTimeout bounds the pass. The rotator holds the Otto slot across the
	// flush (so it can't race a live turn), which means a hung flush would make
	// Otto unavailable — a returning user would get Toto's busy-fallback
	// instead. Rotation only fires after >= 15 minutes of silence, so the odds
	// of overlapping a real message are low, but the bound keeps the worst case
	// short rather than relying on that.
	flushTimeout = 90 * time.Second

	// flushMinTokens is the session size below which flushing isn't worth a
	// subprocess. A session this small is a couple of exchanges — if it held a
	// durable fact, inline memory_add almost certainly caught it already.
	flushMinTokens = 5000

	// flushMaxFacts caps how many entries one flush may add. The core is
	// bounded (2200/1375 chars) and errors at 80% of cap, so an over-eager
	// flush would burn the budget on one session's trivia and start failing
	// the user's own memory_add calls.
	flushMaxFacts = 3
)

// flushAllowedTools restricts the pass to writing memory. Notably absent:
// memory_replace and memory_remove — a background pass that no one reads
// should never be able to delete or rewrite something the user deliberately
// taught Otto. Adding is recoverable; overwriting is not.
var flushAllowedTools = []string{"mcp__otto-memory__memory_add"}

// flushPrompt is the user-side instruction for the distillation turn. It runs
// against the resumed session, so "the conversation above" is literally in
// context — no retrieval needed.
const flushPrompt = `This conversation is ending and its context is about to be cleared.

Before it goes: look back over it and decide whether anything in it is worth
remembering permanently.

Worth remembering (use memory_add):
  • A stable preference the user expressed ("I use fish, not bash")
  • A durable fact about their environment, projects, or tooling
  • A decision or convention they settled on that should hold next time
  • Something they corrected you about

NOT worth remembering:
  • What was discussed, as a topic ("we talked about the deploy")
  • Anything already in the PERSISTENT MEMORY block in your system prompt
  • One-off task details, transient state, or anything time-bound
  • Speculation about what they might want

Call memory_add once per fact, at most ` + flushMaxFactsStr + `, each a single dense
declarative sentence. Target "user" for identity and preferences, "memory" for
environment, projects, and lessons.

If nothing meets that bar — which is the common case — call nothing at all.
An empty flush is a correct flush; do not pad.

Reply with only a one-line summary of what you saved (or "nothing to save").
The user will not see your reply.`

// flushMaxFactsStr keeps the prompt's stated cap in sync with flushMaxFacts.
const flushMaxFactsStr = "three"

// flushSystemPrompt frames the pass. It deliberately does NOT include Otto's
// persona: this is not a conversational turn and nothing here reaches the user,
// so the cat/akhlaq/formatting scaffolding would only be tokens. The memory
// core IS injected, so the model can see what's already stored and skip
// duplicates (memory.Core.Add also rejects exact dupes as a backstop).
const flushSystemPrompt = `You are performing a background memory-flush pass. No human is reading your
reply; the only thing that persists from this turn is what you write via
memory_add.

Be conservative. A wrong or trivial fact in the memory core is worse than a
missing one: the core is injected into every future prompt, it is capacity-
bounded, and nobody reviews it. When in doubt, save nothing.`

// runFlush executes one distillation pass over the session about to be cleared.
// Best-effort by design: any failure is logged and swallowed so a flush problem
// can never block the rotation it precedes. Returns whether the pass ran to
// completion (used only for logging).
//
// The caller must already hold the Otto slot — this resumes Otto's live session,
// so a concurrent turn would interleave.
func (h *handler) runFlush(ctx context.Context, sessionID string, tokens int) bool {
	if h.runner == nil || sessionID == "" {
		return false
	}

	ctx, cancel := context.WithTimeout(ctx, flushTimeout)
	defer cancel()

	events := make(chan claude.Event, 32)
	done := make(chan struct{})
	var summary strings.Builder
	var lastResult claude.ResultEvent
	var gotResult bool

	go func() {
		defer close(done)
		for ev := range events {
			switch e := ev.(type) {
			case claude.AssistantTextEvent:
				summary.WriteString(e.Text)
			case claude.ResultEvent:
				lastResult = e
				gotResult = true
			}
		}
	}()

	err := h.runner.Run(ctx, claude.RunArgs{
		Prompt:    flushPrompt,
		SessionID: sessionID,
		Model:     flushModel,
		// memory_add only — see flushAllowedTools.
		AllowedTools:       flushAllowedTools,
		Source:             "flush",
		AppendSystemPrompt: composePromptWithTimeAndMemory(flushSystemPrompt, h.mem),
		Events:             events,
	})
	close(events)
	<-done

	if gotResult {
		recordUsage(ctx, h.store, "flush", flushModel, lastResult)
	}
	if err != nil {
		// Includes the timeout case. Rotation continues regardless: a session
		// that should be cleared still gets cleared.
		log.Printf("rotator: flush failed (session=%s tokens=%d): %v", sessionID, tokens, err)
		return false
	}

	// The session ID is deliberately NOT re-captured here. The flush adds a
	// turn to a session we are about to clear, so any new id it reports is
	// about to be discarded — writing it back would race the Clear() that
	// follows and could resurrect the session we just distilled.
	log.Printf("rotator: flushed session %s (tokens=%d): %s",
		sessionID, tokens, truncate(strings.TrimSpace(summary.String()), 120))
	return true
}

// shouldFlush reports whether a session about to be rotated is worth a
// distillation pass. Gated on enablement, a wired memory core (nothing to
// write to otherwise), and a session large enough to plausibly hold something
// inline memory_add missed.
func shouldFlush(enabled bool, memWired bool, tokens int) bool {
	return enabled && memWired && tokens >= flushMinTokens
}
