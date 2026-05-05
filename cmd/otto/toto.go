//go:build unix

package main

import (
	"context"
	_ "embed"
	"html"
	"log"
	"math/rand"
	"strings"
	"sync"

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

	mu sync.Mutex // serializes Toto's own --resume against the toto session
}

// Reply runs a Toto turn and sends the result to chatID. ottoPrompt is the
// in-flight Otto prompt (so Toto can refer to it). ottoSnippet is the tail
// of what Otto has streamed so far this turn — partial assistant text Otto
// is mid-way through emitting — so Toto can ground replies in real
// progress instead of hand-waving. userMessage is what the user just sent
// that arrived while Otto was busy.
//
// Toto is invoked with no MCP config and --disallowedTools "*", so even if
// the model tried to call a tool, Claude Code would refuse. Belt-and-
// suspenders against any prompt-injected behaviour.
func (t *Toto) Reply(ctx context.Context, chatID int64, userMessage, ottoPrompt, ottoSnippet string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	systemPrompt := t.persona
	if ottoPrompt != "" {
		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "OTTO IS CURRENTLY WORKING ON THIS FOR THE USER:\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		systemPrompt += ottoPrompt
	}
	if ottoSnippet != "" {
		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "WHAT OTTO HAS PARTIALLY SAID SO FAR (in-progress, the tail of his streamed reply — NOT a finished answer):\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		systemPrompt += ottoSnippet
		systemPrompt += "\n\nUse this only to ground your reply in reality — e.g. 'he's typing about your gmail right now' if the snippet is about gmail. Do NOT relay Otto's words to the user verbatim or pretend his answer is yours."
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

	// Toto's runner has empty configured systemPrompt and mcpConfigPath
	// (set in main.go). We inject the per-call persona + Otto-context
	// via AppendSystemPrompt so the system prompt can change between
	// calls based on what Otto is currently working on.
	err := t.runner.Run(ctx, claude.RunArgs{
		Prompt:             userMessage,
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

	if err != nil {
		// Toto failing is annoying but not the end of the world — fall back
		// to a hardcoded message so the user still gets *something* during
		// Otto's busy window.
		t.send(ctx, chatID, "mrow. (toto's having a moment, otto's still busy)")
		log.Printf("toto run error: %v", err)
		return
	}

	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		out = "mrrp."
	}
	out = stripMarkdown(out)
	t.send(ctx, chatID, out)
	// Note: we deliberately ignore PermissionDenials for Toto — with
	// --disallowedTools "*" any tool attempt is denied by design, and we
	// don't want to surface inline-keyboard buttons asking the user to
	// approve tools for the no-tools persona.
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
	sb.WriteString("<blockquote>🐱 <b>TOTO</b></blockquote>\n")
	if art != "" {
		sb.WriteString("<pre>")
		sb.WriteString(html.EscapeString(art))
		sb.WriteString("</pre>\n\n")
	}
	sb.WriteString(escapedBody)
	if err := t.bot.SendMessageHTML(ctx, chatID, sb.String()); err != nil {
		log.Printf("toto html send error: %v (falling back to plain)", err)
		plain := "🐱 TOTO\n\n" + body
		if err2 := telegram.SendChunked(ctx, t.bot, chatID, plain); err2 != nil {
			log.Printf("toto plain send error: %v", err2)
		}
	}
}
