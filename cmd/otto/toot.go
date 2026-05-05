//go:build unix

package main

import (
	"context"
	_ "embed"
	"fmt"
	"html"
	"log"
	"strings"
	"sync"

	"otto/internal/claude"
	"otto/internal/telegram"
)

// tootModel pins Toot to Sonnet — he's smarter than Toto (haiku) on
// purpose. He thinks before composing changelog summaries.
const tootModel = "claude-sonnet-4-6"

// tootEffort is the reasoning budget passed to Claude Code as --effort.
// Medium gives Toot a few moments to organize the changelog without
// burning a huge token budget on every release.
const tootEffort = "medium"

// tootArtFile is the bundled owl ASCII-art file. Three blocks separated
// by blank lines; same format the existing parseAsciiArts (in toto.go)
// already handles for Toto's cats.
//
//go:embed toot.txt
var tootArtFile string

// tootCycler hands out the embedded owl arts in shuffled round-robin
// order, so consecutive Toot messages don't repeat the same art.
var tootCycler = newAsciiCycler(parseAsciiArts(tootArtFile))

// pickTootArt returns the next owl art via the shuffled round-robin
// cycler, or "" if no arts were loaded.
func pickTootArt() string { return tootCycler.Next() }

// Toot is the owl character that delivers update notifications. He
// reads patch notes and explains them in his own voice (nerdy,
// systematic, dutiful). Mirrors Toto's architecture — own runner,
// session, persona — but uses a smarter model and a different prompt.
//
// Toot's tools are all denied (--disallowedTools "*") so even though
// he runs through Claude Code, he can't touch the filesystem, MCPs,
// or anything else. He talks. That's it.
//
// Conversational messages (command replies, error messages) stay on
// the regular bot — Toot exists specifically to mark "this is an
// update event" visually so the user knows what kind of message
// they're reading.
type Toot struct {
	bot     telegram.BotClient
	runner  claude.Runner
	session *claude.Session
	persona string // base system prompt for Toot (TOOT.md content)

	mu sync.Mutex // serializes Toot's own --resume against the toot session
}

// Name returns "toot" — used by the petRegistry to route messages
// addressed to him directly.
func (t *Toot) Name() string { return "toot" }

// Reply runs a chat turn — the user addressed Toot directly. Stays in
// his nerdy/dutiful voice but engages conversationally rather than
// reciting changelog. Tools remain disallowed; Toot can talk, that's it.
func (t *Toot) Reply(ctx context.Context, chatID int64, userMessage string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	systemPrompt := t.persona
	systemPrompt += "\n\n───────────────────────────────────────────────\n"
	systemPrompt += "THE USER ADDRESSED YOU DIRECTLY (CHAT MODE).\n"
	systemPrompt += "───────────────────────────────────────────────\n\n"
	systemPrompt += "This is not a release announcement. They want to talk to YOU. Stay in your voice — dutiful, formal-ish, dryly nerdy — but engage. You may discuss Otto, Toto, releases, your job, whatever they bring up. Decline tool requests politely (you only talk). Keep replies brief; phone-screen friendly."

	prompt := userMessage
	if prompt == "" {
		prompt = "(the user pinged you with no content — likely a greeting or attention check)"
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

	err := t.runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		SessionID:          t.session.ID(),
		Model:              tootModel,
		Effort:             tootEffort,
		DisallowedTools:    []string{"*"},
		AppendSystemPrompt: systemPrompt,
		Events:             events,
	})
	close(events)
	<-doneParsing

	if capturedSessionID != "" && capturedSessionID != t.session.ID() {
		if setErr := t.session.Set(capturedSessionID); setErr != nil {
			log.Printf("toot session save: %v", setErr)
		}
	}

	if err != nil {
		log.Printf("toot reply error: %v", err)
		systemErr(ctx, t.bot, chatID, "⚠️ Toot couldn't reply right now (claude error). Try again in a moment.")
		return
	}
	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		log.Printf("toot reply: empty output")
		systemErr(ctx, t.bot, chatID, "⚠️ Toot returned an empty reply. Try again.")
		return
	}
	out = stripMarkdown(out)
	_ = t.deliver(ctx, chatID, out)
}

// Announce composes a release notification in Toot's voice and sends
// it. body is the GitHub release notes (auto-generated changelog when
// generate_release_notes: true). Toot reads them, explains the items,
// and signs off in character.
func (t *Toot) Announce(ctx context.Context, chatID int64, currentVersion, newTag, body string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	systemPrompt := t.persona
	if systemPrompt != "" {
		systemPrompt += "\n\n"
	}
	systemPrompt += "───────────────────────────────────────────────\n"
	systemPrompt += "RELEASE TO ANNOUNCE\n"
	systemPrompt += "───────────────────────────────────────────────\n\n"
	systemPrompt += fmt.Sprintf("Current version installed: %s\n", currentVersion)
	systemPrompt += fmt.Sprintf("New version available:     %s\n\n", newTag)
	if body != "" {
		systemPrompt += "Patch notes from the release:\n\n"
		systemPrompt += body + "\n\n"
	} else {
		systemPrompt += "No patch notes were attached to this release.\n\n"
	}
	systemPrompt += "Compose your announcement now. End by reminding the user to reply /update to install. Keep it concise — phone-screen friendly."

	prompt := fmt.Sprintf("Announce release %s.", newTag)

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

	err := t.runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		SessionID:          t.session.ID(),
		Model:              tootModel,
		Effort:             tootEffort,
		DisallowedTools:    []string{"*"},
		AppendSystemPrompt: systemPrompt,
		Events:             events,
	})
	close(events)
	<-doneParsing

	if capturedSessionID != "" && capturedSessionID != t.session.ID() {
		if setErr := t.session.Set(capturedSessionID); setErr != nil {
			log.Printf("toot session save: %v", setErr)
		}
	}

	// Runner failed or returned nothing. We don't fake an announcement
	// in Toot's voice; instead we send a plain system message so the
	// user still learns about the release without a fabricated quote.
	if err != nil {
		log.Printf("toot announce error: %v", err)
		systemErr(ctx, t.bot, chatID, fmt.Sprintf(
			"⚠️ Toot couldn't compose the release announcement (claude error). Update available: %s → %s. Reply /update to install.",
			currentVersion, newTag,
		))
		return nil
	}
	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		log.Printf("toot announce: empty output")
		systemErr(ctx, t.bot, chatID, fmt.Sprintf(
			"⚠️ Toot returned an empty announcement. Update available: %s → %s. Reply /update to install.",
			currentVersion, newTag,
		))
		return nil
	}
	out = stripMarkdown(out)
	return t.deliver(ctx, chatID, out)
}

// Confirm sends the post-install "restarting" message in Toot's voice.
// Goes through Claude — same authenticity rule as Announce/Reply: every
// line attributed to Toot is real LLM output. The extra latency (~5s)
// is acceptable since Confirm fires once per install (rare).
func (t *Toot) Confirm(ctx context.Context, chatID int64, tag string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	systemPrompt := t.persona
	systemPrompt += "\n\n───────────────────────────────────────────────\n"
	systemPrompt += "INSTALL CONFIRMATION\n"
	systemPrompt += "───────────────────────────────────────────────\n\n"
	systemPrompt += fmt.Sprintf("Otto just finished installing version %s. The process is about to restart so Otto can come back online running the new binary.\n\n", tag)
	systemPrompt += "Compose a brief one-or-two-sentence installation-complete message in your voice — confirm the version is in place, note the upcoming restart. Terse. No bullets needed."

	prompt := fmt.Sprintf("Confirm install of %s.", tag)

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

	err := t.runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		SessionID:          t.session.ID(),
		Model:              tootModel,
		Effort:             tootEffort,
		DisallowedTools:    []string{"*"},
		AppendSystemPrompt: systemPrompt,
		Events:             events,
	})
	close(events)
	<-doneParsing

	if capturedSessionID != "" && capturedSessionID != t.session.ID() {
		if setErr := t.session.Set(capturedSessionID); setErr != nil {
			log.Printf("toot session save: %v", setErr)
		}
	}

	if err != nil {
		log.Printf("toot confirm error: %v", err)
		systemErr(ctx, t.bot, chatID, fmt.Sprintf("⚠️ Installed %s, restarting now.", tag))
		return nil
	}
	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		log.Printf("toot confirm: empty output")
		systemErr(ctx, t.bot, chatID, fmt.Sprintf("⚠️ Installed %s, restarting now.", tag))
		return nil
	}
	out = stripMarkdown(out)
	return t.deliver(ctx, chatID, out)
}

// deliver wraps body with Toot's banner + a random owl art and sends
// via HTML mode. Banner format (blockquote + bold all-caps + emoji)
// mirrors Toto's send method so the two characters render with the
// same visual structure but different colors / emoji.
func (t *Toot) deliver(ctx context.Context, chatID int64, body string) error {
	art := pickTootArt()
	escapedBody := html.EscapeString(body)
	var sb strings.Builder
	sb.WriteString("<blockquote><b>TOOT</b></blockquote>\n")
	if art != "" {
		sb.WriteString("<pre>")
		sb.WriteString(html.EscapeString(art))
		sb.WriteString("</pre>\n\n")
	}
	sb.WriteString(escapedBody)
	if err := t.bot.SendMessageHTML(ctx, chatID, sb.String()); err != nil {
		// HTML send failure (rare) falls back to plain text so the
		// content still reaches the user, banner and all.
		log.Printf("toot html send error: %v (falling back to plain)", err)
		plain := "TOOT\n\n" + body
		return telegram.SendChunked(ctx, t.bot, chatID, plain)
	}
	return nil
}
