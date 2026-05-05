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

// totoArts is populated at init() from totoArtFile.
var totoArts = parseAsciiArts(totoArtFile)

// asciiCycler hands out arts in a shuffled round-robin: every art appears
// exactly once before any repeats, then we reshuffle. Removes the chance
// that a few rolls of rand.Intn happen to all hit the same index, which
// makes the bot feel "stuck" on one cat from the user's perspective.
var asciiCycler = newAsciiCycler(totoArts)

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

	// ottoStatus, when non-nil, returns a snapshot of Otto's current
	// state. Toto includes this in his per-call system prompt so he can
	// answer "what's otto up to?" honestly. Production callers wire it
	// to handler.otto.Snapshot; tests leave it nil.
	ottoStatus func() ottoSnapshot

	mu sync.Mutex // serializes Toto's own --resume against the toto session
}

// Name returns "toto" — used by the petRegistry to route messages
// addressed to him directly.
func (t *Toto) Name() string { return "toto" }

// Reply runs a Toto turn for a direct-address message — the user
// said "toto, ..." (or similar) and we routed it here. The
// per-call prompt includes Otto's current status (if available) so
// Toto can answer "what's otto doing?" truthfully.
func (t *Toto) Reply(ctx context.Context, chatID int64, userMessage string) {
	t.replyWithContext(ctx, chatID, userMessage, false, "", "")
}

// BusyReply runs a Toto turn for the busy-fallback path — Otto is
// mid-task and the user sent another message. ottoPrompt and
// ottoSnippet ground Toto's reply in what Otto's actually working on.
func (t *Toto) BusyReply(ctx context.Context, chatID int64, userMessage, ottoPrompt, ottoSnippet string) {
	t.replyWithContext(ctx, chatID, userMessage, true, ottoPrompt, ottoSnippet)
}

// replyWithContext is the shared implementation for both paths.
// busyFallback distinguishes the two modes: in busy-fallback the user's
// message wasn't addressed to Toto and Otto's the one they meant; in
// direct-address Toto is who they asked for. The Otto-status snippet
// (busy/idle/working-on-X) is injected in BOTH modes so Toto always
// knows what's going on.
//
// Toto is invoked with no MCP config and --disallowedTools "*", so
// even if the model tried to call a tool, Claude Code would refuse.
func (t *Toto) replyWithContext(ctx context.Context, chatID int64, userMessage string, busyFallback bool, ottoPrompt, ottoSnippet string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	systemPrompt := t.persona
	if busyFallback {
		// User's message was meant for Otto, but Otto is busy. ottoPrompt
		// and ottoSnippet were captured by dispatch under lock and reflect
		// the same data the snapshot would yield.
		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "OTTO IS CURRENTLY WORKING ON THIS FOR THE USER:\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		systemPrompt += ottoPrompt
		if ottoSnippet != "" {
			systemPrompt += "\n\n───────────────────────────────────────────────\n"
			systemPrompt += "WHAT OTTO HAS PARTIALLY SAID SO FAR (in-progress, the tail of his streamed reply — NOT a finished answer):\n"
			systemPrompt += "───────────────────────────────────────────────\n\n"
			systemPrompt += ottoSnippet
			systemPrompt += "\n\nUse this only to ground your reply in reality — e.g. 'he's typing about your gmail right now' if the snippet is about gmail. Do NOT relay Otto's words to the user verbatim or pretend his answer is yours."
		}
	} else {
		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "THE USER ADDRESSED YOU DIRECTLY.\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		systemPrompt += "They want to talk to YOU, not Otto. They specifically said your name. Greet them like a cat. Chat in your voice."

		// Status snippet so Toto can answer "what's otto up to?" with truth.
		if t.ottoStatus != nil {
			snap := t.ottoStatus()
			systemPrompt += "\n\n───────────────────────────────────────────────\n"
			systemPrompt += "OTTO STATUS (in case the user asks):\n"
			systemPrompt += "───────────────────────────────────────────────\n\n"
			if snap.Busy {
				systemPrompt += "Otto is BUSY. He's currently working on:\n\n"
				systemPrompt += "  " + snap.CurrentPrompt
				if snap.Snippet != "" {
					systemPrompt += "\n\nTail of his in-progress reply (don't quote verbatim — just for grounding):\n\n  " + snap.Snippet
				}
				if snap.Silence > 30*time.Second {
					systemPrompt += fmt.Sprintf("\n\n(He's been silent for %s.)", snap.Silence.Round(time.Second))
				}
			} else {
				systemPrompt += "Otto is IDLE. Nothing in progress."
			}
			systemPrompt += "\n\nMention this only if the user asks or if it's clearly relevant. Don't volunteer it unprompted."
		}
	}

	events := make(chan claude.Event, 32)
	doneParsing := make(chan struct{})
	var assistantText strings.Builder
	var capturedSessionID string

	go func() {
		defer close(doneParsing)
		for ev := range events {
			switch e := ev.(type) {
			case claude.AssistantTextEvent:
				assistantText.WriteString(e.Text)
			case claude.SessionEvent:
				capturedSessionID = e.ID
			}
		}
	}()

	// Direct-address pings can have an empty body ("toto"). Send a
	// minimal user-side prompt so Claude has something to react to.
	prompt := userMessage
	if prompt == "" {
		prompt = "(the user pinged you with no content — likely a greeting or attention check)"
	}

	err := t.runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		SessionID:          t.session.ID(),
		Model:              totoModel,
		DisallowedTools:    []string{"*"},
		AppendSystemPrompt: systemPrompt,
		Events:             events,
	})
	close(events)
	<-doneParsing

	if capturedSessionID != "" && capturedSessionID != t.session.ID() {
		if setErr := t.session.Set(capturedSessionID); setErr != nil {
			log.Printf("toto session save: %v", setErr)
		}
	}

	// On runner error or empty output, we deliberately do NOT fake a
	// reply in Toto's voice. Instead we send a plain system message via
	// the bot so the user knows Toto wasn't reached — every line that
	// goes through Toto's banner is real LLM output in his voice.
	if err != nil {
		log.Printf("toto run error: %v", err)
		systemErr(ctx, t.bot, chatID, "⚠️ Toto couldn't reply right now (claude error). Try again in a moment.")
		return
	}
	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		log.Printf("toto: empty output")
		systemErr(ctx, t.bot, chatID, "⚠️ Toto returned an empty reply. Try again.")
		return
	}
	out = stripMarkdown(out)
	t.send(ctx, chatID, out)
}

// systemErr sends a plain (no-banner) system message — used when a pet
// can't reply authentically and we need to tell the user without
// pretending the pet spoke. Failure of this send is logged but not
// surfaced; we're already on an error path.
func systemErr(ctx context.Context, bot telegram.BotClient, chatID int64, msg string) {
	if err := telegram.SendChunked(ctx, bot, chatID, msg); err != nil {
		log.Printf("system error msg send: %v", err)
	}
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
