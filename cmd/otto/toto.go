//go:build unix

package main

import (
	"context"
	_ "embed"
	"fmt"
	"html"
	"log"
	"math/rand"
	"strings"
	"sync"
	"time"

	"otto/internal/claude"
	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
	"otto/internal/telegram"
)

// totoModel pins Toto to a small fast model. Toto's whole purpose is "be
// quick and not be Otto"; the heavyweight model belongs to Otto.
const totoModel = "claude-haiku-4-5"

// totoArtFile is the bundled ASCII-art file shipped alongside this binary.
// One block (separated by a blank line from the next) is randomly picked
// to head every Toto reply.
//
//go:embed toto.txt
var totoArtFile string

// asciiCycler hands out arts in a shuffled round-robin: every art appears
// exactly once before any repeats, then we reshuffle. Removes the chance
// that a few rolls of rand.Intn happen to all hit the same index, which
// makes the bot feel "stuck" on one cat from the user's perspective.
var asciiCycler = newAsciiCycler(parseAsciiArts(totoArtFile))

type asciiRoundRobin struct {
	mu     sync.Mutex
	arts   []string
	order  []int // shuffled indices into arts
	cursor int
}

func newAsciiCycler(arts []string) *asciiRoundRobin {
	c := &asciiRoundRobin{arts: arts}
	c.reshuffle()
	return c
}

func (c *asciiRoundRobin) reshuffle() {
	c.order = make([]int, len(c.arts))
	for i := range c.order {
		c.order[i] = i
	}
	rand.Shuffle(len(c.order), func(i, j int) {
		c.order[i], c.order[j] = c.order[j], c.order[i]
	})
	c.cursor = 0
}

func (c *asciiRoundRobin) Next() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.arts) == 0 {
		return ""
	}
	if c.cursor >= len(c.order) {
		c.reshuffle()
	}
	art := c.arts[c.order[c.cursor]]
	c.cursor++
	return art
}

// parseAsciiArts splits raw ASCII art file content on blank-line boundaries.
// Each non-empty block becomes one art. Leading/trailing whitespace inside
// a block is preserved (the visual layout matters).
//
// Tabs are expanded to 8 spaces because Telegram's <pre> rendering treats
// a tab as one narrow glyph rather than the conventional tab-stop width,
// which silently breaks any art that mixes tabs with spaces for
// indentation. 8 matches the de-facto editor default the art was likely
// authored against.
func parseAsciiArts(raw string) []string {
	raw = strings.ReplaceAll(raw, "\t", "        ")
	var arts []string
	var cur strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			if cur.Len() > 0 {
				arts = append(arts, strings.TrimRight(cur.String(), "\n"))
				cur.Reset()
			}
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if cur.Len() > 0 {
		arts = append(arts, strings.TrimRight(cur.String(), "\n"))
	}
	return arts
}

// pickAsciiArt returns the next ASCII art block via the shuffled round-
// robin cycler, or "" if no arts were loaded.
func pickAsciiArt() string { return asciiCycler.Next() }

// Toto wraps a claude.Runner configured for the lightweight cat-themed
// fallback persona. It owns its own session ID (separate from Otto's) so
// the two personas don't pollute each other's conversation memory, and a
// mutex serializing its own --resume against itself.
type Toto struct {
	bot     telegram.BotClient
	runner  claude.Runner
	session *claude.Session
	persona string // base system prompt for Toto (TOTO.md content)

	mem      *memory.Core   // injected into Toto's prompt; nil disables
	store    *store.Store   // turn log; nil disables
	embedder embed.Embedder // embeds turns for semantic search; nil disables

	// ottoStatus, when non-nil, returns a snapshot of Otto's current
	// state. Toto includes this in his per-call system prompt so he can
	// answer "what's otto up to?" honestly. Production callers wire it
	// to handler.otto.Snapshot; tests leave it nil.
	ottoStatus func() ottoSnapshot

	mu         sync.Mutex // serializes Toto's own --resume against the toto session
	lastActive time.Time  // last reply time; drives idle session rotation (guarded by mu)
}

// rotateIfIdle clears Toto's session if it has gone idle for at least window,
// mirroring Otto's idle reset. Pet sessions otherwise live forever and answer
// from stale history (e.g. an old version number). TryLock means this can
// never race a live reply: when a Reply is in flight (it holds mu for the
// whole Claude subprocess run, often minutes) we skip and genuinely defer
// the clear to the next tick instead of parking the shared rotator
// goroutine — which would also stall Otto's own session rotation.
func (t *Toto) rotateIfIdle(window time.Duration) {
	if !t.mu.TryLock() {
		return
	}
	defer t.mu.Unlock()
	if t.session.ID() == "" || t.lastActive.IsZero() {
		return
	}
	if idle := time.Since(t.lastActive); idle >= window {
		if err := t.session.Clear(); err != nil {
			log.Printf("rotator: clear toto session: %v", err)
			return
		}
		log.Printf("rotator: cleared toto session (idle %s)", idle.Round(time.Second))
	}
}

// Name returns "toto" — used by the petRegistry to route messages
// addressed to him directly.
func (t *Toto) Name() string { return "toto" }

// Reply runs a Toto turn for a direct-address message — the user
// said "toto, ..." (or similar) and we routed it here. The
// per-call prompt includes Otto's current status (if available) so
// Toto can answer "what's otto doing?" truthfully.
func (t *Toto) Reply(ctx context.Context, chatID int64, userMessage string) {
	t.replyWithContext(ctx, chatID, userMessage, false, "", "", nil, nil)
}

// BusyReply runs a Toto turn for the busy-fallback path — Otto is
// mid-task and the user sent another message. ottoPrompt and
// ottoSnippet ground Toto's reply in what Otto's actually working on;
// activity carries the tool calls behind that work, which during a long
// agentic turn is the only signal that isn't stale.
func (t *Toto) BusyReply(ctx context.Context, chatID int64, userMessage, ottoPrompt, ottoSnippet string, activity []activityEntry) {
	t.replyWithContext(ctx, chatID, userMessage, true, ottoPrompt, ottoSnippet, activity, nil)
}

// BusReply runs a Toto turn for a message that arrived via the inbox
// from another agent. The per-call system prompt grows a BUS CONTEXT +
// HOPS REMAINING block so Toto can keep the loop alive via
// message_<sender> or wind down naturally on the last hop. The bus env
// vars are stamped on the underlying claude subprocess so the MCP tool
// handlers know who Toto is and how deep the chain is.
func (t *Toto) BusReply(ctx context.Context, chatID int64, body string, bc busContext) {
	t.replyWithContext(ctx, chatID, body, false, "", "", nil, &bc)
}

// totoAllowedTools is the closed allowlist of MCP tools Toto may call.
// Everything else is blocked, including built-in filesystem and shell.
// forward_to_otto lets Toto hand actual work off to Otto via the inbox
// bus; message_toot lets Toto DM the owl directly when functional
// (release-shaped) pings warrant it; session_search lets Toto recall
// past turns when needed. The scoped mcp.json restricts what's even
// reachable; this allowlist tightens it further to specific tool names.
var totoAllowedTools = []string{
	"mcp__otto-memory__forward_to_otto",
	"mcp__otto-memory__message_toot",
	"mcp__otto-memory__session_search",
	"mcp__otto-memory__recent_turns",
}

// statusNoteCap bounds how many runes of Otto's prompt / reply-tail each
// parenthetical status line carries. The note is a stage direction, not a
// transcript — it needs to convey the gist ("he's on your gmail") in a line
// the model reads at a glance. The snippet is already byte-capped upstream
// at snippetCap; this bounds the visible line further.
const statusNoteCap = 200

// flattenForNote collapses s to a single line (newlines and runs of
// whitespace become one space) and truncates it to max runes. Status notes
// are line-oriented parentheticals — an embedded newline would break the
// "one fact per line" reading the model relies on.
func flattenForNote(s string, max int) string {
	return truncate(strings.Join(strings.Fields(s), " "), max)
}

// ottoStatusNote renders the live Otto-status stage direction prepended to
// Toto's user-side prompt.
//
// This deliberately lives on the USER side, not in --append-system-prompt.
// Toto runs with --resume, so his conversation history persists; a small
// model weighs a prior assistant turn ("idle. nothing.") over a system
// prompt that changed silently between turns, and answers "what's he doing
// now?" by echoing its own last answer. Putting the live values inline in
// the user message means the history itself records the status at each turn
// — the model sees "last turn: idle → I said idle; this turn: busy on X"
// and updates naturally.
//
// Returns "" when there is nothing to report, so callers can prepend
// unconditionally.
func ottoStatusNote(busy bool, ottoPrompt, ottoSnippet string, silence time.Duration) string {
	var lines []string
	if busy {
		if p := flattenForNote(ottoPrompt, statusNoteCap); p != "" {
			lines = append(lines, fmt.Sprintf("(otto status: busy on %q)", p))
		} else {
			lines = append(lines, "(otto status: busy)")
		}
		if s := flattenForNote(ottoSnippet, statusNoteCap); s != "" {
			lines = append(lines, fmt.Sprintf("(tail of his reply: %q)", s))
		}
		if silence > 30*time.Second {
			lines = append(lines, fmt.Sprintf("(he's been silent for %s)", silence.Round(time.Second)))
		}
	} else {
		lines = append(lines, "(otto status: idle)")
	}
	return strings.Join(lines, "\n")
}

// replyWithContext is the shared implementation for both paths.
// busyFallback distinguishes the two modes: in busy-fallback the user's
// message wasn't addressed to Toto and Otto's the one they meant; in
// direct-address Toto is who they asked for. The Otto-status snippet
// (busy/idle/working-on-X) is injected in BOTH modes so Toto always
// knows what's going on.
//
// bc non-nil means this call originated from the agent bus, not a real
// user message. Bus-relay turns are NOT logged: the turn store is the
// source-of-truth for session_search, and writing agent-to-agent payloads
// there under the "toto/user" persona would cause semantic search to surface
// relay bodies as if they were user-authored messages.
//
// Toto runs with a Toto-scoped mcp.json (only otto-memory) plus an
// explicit --allowedTools allowlist of forward_to_otto + session_search.
// He can't reach gmail/notion/etc., and he can't call any built-in tool.
func (t *Toto) replyWithContext(ctx context.Context, chatID int64, userMessage string, busyFallback bool, ottoPrompt, ottoSnippet string, activity []activityEntry, bc *busContext) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastActive = time.Now()

	systemPrompt := t.persona
	if bc != nil {
		// Bus-sourced turn: prepend the BUS CONTEXT + HOPS REMAINING block
		// so Toto knows he's mid-chain and how to keep / wrap the loop.
		systemPrompt += "\n\n" + busPromptBlock(*bc, "toto")
	}
	// statusNote carries the live, volatile Otto state. It is built here but
	// prepended to the USER-side prompt below — see ottoStatusNote for why it
	// must not live in the system prompt.
	var statusNote string
	if busyFallback {
		// User's message was meant for Otto, but Otto is busy. ottoPrompt
		// and ottoSnippet were captured by dispatch under lock and reflect
		// the same data the snapshot would yield.
		statusNote = ottoStatusNote(true, ottoPrompt, ottoSnippet, 0)

		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "OTTO IS CURRENTLY WORKING ON THIS FOR THE USER.\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		systemPrompt += "What he's working on — and the tail of his in-progress reply — "
		systemPrompt += "are in the status note at the top of the user's message. That note "
		systemPrompt += "is the CURRENT truth; re-read it every turn rather than repeating "
		systemPrompt += "what you said last time.\n\n"
		systemPrompt += "Use it only to ground your reply in reality — e.g. 'he's typing about "
		systemPrompt += "your gmail right now' if the tail is about gmail. Do NOT relay Otto's "
		systemPrompt += "words to the user verbatim, pretend his answer is yours, or echo the "
		systemPrompt += "note itself back."
		if block := formatActivityForPet(activity); block != "" {
			systemPrompt += "\n\n───────────────────────────────────────────────\n"
			systemPrompt += block
		}
	} else {
		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "THE USER ADDRESSED YOU DIRECTLY.\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		systemPrompt += "They want to talk to YOU, not Otto. They specifically said your name. Greet them like a cat. Chat in your voice."

		// Status note so Toto can answer "what's otto up to?" with truth.
		if t.ottoStatus != nil {
			snap := t.ottoStatus()
			statusNote = ottoStatusNote(snap.Busy, snap.CurrentPrompt, snap.Snippet, snap.Silence)

			systemPrompt += "\n\n───────────────────────────────────────────────\n"
			systemPrompt += "OTTO STATUS (in case the user asks):\n"
			systemPrompt += "───────────────────────────────────────────────\n\n"
			systemPrompt += "The status note at the top of the user's message says what Otto is "
			systemPrompt += "doing right now — either busy on a specific prompt, or idle. Read it "
			systemPrompt += "LITERALLY and re-read it every turn: it changes between messages, and "
			systemPrompt += "your own previous answer is NOT evidence of the current state.\n\n"
			systemPrompt += "Don't improvise verbs like \"pulling\" or \"offline\". Paraphrase the "
			systemPrompt += "note when asked; never invent, never echo the note back verbatim."
			if block := formatActivityForPet(snap.Activity); block != "" {
				systemPrompt += "\n\n───────────────────────────────────────────────\n"
				systemPrompt += block
			}
		}
	}

	systemPrompt = composePromptWithTimeAndMemory(systemPrompt, t.mem)

	events := make(chan claude.Event, 32)
	doneParsing := make(chan struct{})
	var assistantText strings.Builder
	var capturedSessionID string
	var lastResult claude.ResultEvent
	var gotResult bool

	go func() {
		defer close(doneParsing)
		for ev := range events {
			switch e := ev.(type) {
			case claude.AssistantTextEvent:
				assistantText.WriteString(e.Text)
			case claude.SessionEvent:
				capturedSessionID = e.ID
			case claude.ResultEvent:
				lastResult = e
				gotResult = true
			}
		}
	}()

	// Direct-address pings can have an empty body ("toto"). Send a
	// minimal user-side prompt so Claude has something to react to.
	prompt := userMessage
	if prompt == "" {
		prompt = "(the user pinged you with no content — likely a greeting or attention check)"
	}
	// Prepend the live status AFTER the empty-body fallback, so a bare "toto"
	// still carries current Otto state. Empty when there's nothing to report
	// (direct address with no ottoStatus wired), leaving the prompt untouched.
	if statusNote != "" {
		prompt = statusNote + "\n\n" + prompt
	}

	// Cross-process: env vars carry the hop counter and self-name to the
	// MCP server child so its tools enqueue follow-ups at the right depth
	// and stamp the right sender. Stamp unconditionally — even on a direct
	// (non-bus) turn Toto may call message_toot, and without the env its
	// sender would misattribute to "otto" (defaultSenderFor's two-way guess
	// can't name a third agent). hop is 0 when the turn didn't arrive via
	// the bus.
	hop := 0
	if bc != nil {
		hop = bc.Hop
	}
	runner := t.runner.WithEnv(busEnv(hop, "toto"))
	err := runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		SessionID:          t.session.ID(),
		Model:              totoModel,
		AllowedTools:       totoAllowedTools,
		AppendSystemPrompt: systemPrompt,
		Events:             events,
		Source:             "toto",
	})
	close(events)
	<-doneParsing

	if gotResult {
		recordUsage(ctx, t.store, "toto", totoModel, lastResult)
	}

	if capturedSessionID != "" && capturedSessionID != t.session.ID() {
		if setErr := t.session.Set(capturedSessionID); setErr != nil {
			log.Printf("toto session save: %v", setErr)
		}
	}

	if err != nil {
		// Toto failing falls back to a hardcoded message so the user
		// still gets *something*. Voice changes slightly depending on
		// the path.
		fallback := "mrow. (toto's having a moment, otto's still busy)"
		if ottoPrompt == "" && ottoSnippet == "" {
			fallback = "mrow. (sorry, brain not working. try me again later.)"
		}
		t.send(ctx, chatID, fallback)
		log.Printf("toto run error: %v", err)
		return
	}

	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		out = "mrrp."
	}
	out = stripMarkdown(out)
	t.send(ctx, chatID, out)

	// Skip logging for bus-relay turns (bc != nil): the turn store is the
	// source-of-truth for session_search. Storing agent relay payloads
	// under "toto/user" would mix them with real user turns and cause
	// session_search to surface them as user-authored messages.
	if bc == nil {
		logTurn(ctx, t.store, t.embedder, "toto", "user", userMessage)
		logTurn(ctx, t.store, t.embedder, "toto", "assistant", out)
	}
}

// SystemMessage delivers an out-of-band Toto reply (e.g. the watchdog's
// "I rebooted Otto" notification). Acquires t.mu so the message is
// serialized against any in-flight Reply/BusyReply call — without this,
// the user could see the system message interleave with a Toto reply
// that's already mid-flight, and the second message could refer to
// state the first has invalidated.
func (t *Toto) SystemMessage(ctx context.Context, chatID int64, body string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.send(ctx, chatID, body)
}

// send prepends a randomly-chosen ASCII art and sends the result via
// HTML parse mode so the art renders monospace inside <pre>...</pre>.
// Body is HTML-escaped so any literal angle-brackets / ampersands from
// Toto don't break parsing.
//
// Falls back to plain SendMessage if HTML send fails — degraded but the
// user still gets the body.
func (t *Toto) send(ctx context.Context, chatID int64, body string) {
	art := pickAsciiArt()
	escapedBody := html.EscapeString(body)
	var sb strings.Builder
	sb.WriteString("<blockquote><b>TOTO</b></blockquote>\n")
	if art != "" {
		sb.WriteString("<pre>")
		sb.WriteString(html.EscapeString(art))
		sb.WriteString("</pre>\n\n")
	}
	sb.WriteString(escapedBody)
	if err := t.bot.SendMessageHTML(ctx, chatID, sb.String()); err != nil {
		log.Printf("toto html send error: %v (falling back to plain)", err)
		plain := "TOTO\n\n" + body
		if err2 := telegram.SendChunked(ctx, t.bot, chatID, plain); err2 != nil {
			log.Printf("toto plain send error: %v", err2)
		}
	}
}
