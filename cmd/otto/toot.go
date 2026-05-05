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

	// pendingUpdate, when non-nil, returns the current pending release
	// (or nil if none). Surfaced into Toot's chat-mode prompt so he
	// knows whether install requests are valid. Wired to the updater
	// in main.go.
	pendingUpdate func() *pendingUpdate

	// triggerUpdate, when non-nil, asynchronously kicks off the
	// install + restart flow (same as the /update command). Toot calls
	// this when the user has clearly authorized the install during
	// chat. Wired to handler.runUpdate in main.go.
	triggerUpdate func()

	mu sync.Mutex // serializes Toot's own --resume against the toot session
}

// tootUpdateMarker is the literal string Toot's LLM is instructed to
// emit when the user has authorized an install during chat. The
// dispatcher strips it from the visible reply and fires triggerUpdate.
const tootUpdateMarker = "[TRIGGER_UPDATE]"

// stripUpdateMarker removes the install-trigger marker from Toot's
// reply. Returns the cleaned text and a bool indicating whether the
// marker was present.
func stripUpdateMarker(s string) (string, bool) {
	if !strings.Contains(s, tootUpdateMarker) {
		return s, false
	}
	cleaned := strings.ReplaceAll(s, tootUpdateMarker, "")
	return strings.TrimSpace(cleaned), true
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

	// Inject the install_update tool when a release is pending. Framing
	// it as a "tool you can call" (rather than a strict marker rule)
	// nudges Toot toward using judgment on natural phrasings ("can you
	// update", "would you mind installing", etc.) rather than only
	// matching the literal example wordings.
	if t.pendingUpdate != nil {
		systemPrompt += "\n\n───────────────────────────────────────────────\n"
		systemPrompt += "TOOLS AVAILABLE TO YOU\n"
		systemPrompt += "───────────────────────────────────────────────\n\n"
		if p := t.pendingUpdate(); p != nil {
			systemPrompt += fmt.Sprintf("install_update — installs the pending release (%s).\n\n", p.Tag)
			systemPrompt += "To call this tool, end your reply with the literal marker on its own line:\n\n  "
			systemPrompt += tootUpdateMarker
			systemPrompt += "\n\nThe marker is invisible to the user — the system strips it and starts the install. After install completes, the standard \"Installed v…, restarting\" confirmation appears.\n\n"
			systemPrompt += "WHEN TO CALL install_update\n\n"
			systemPrompt += "Use your judgment. If the user's message is *any reasonable form* of asking you to install — direct, polite, colloquial, terse — call the tool. Don't be overly literal:\n\n"
			systemPrompt += "  - \"do it\" / \"update\" / \"install\" / \"go ahead\"\n"
			systemPrompt += "  - \"can you update?\" / \"could you install?\" / \"would you mind?\"\n"
			systemPrompt += "  - \"hey toot can you update\" / \"toot please install\"\n"
			systemPrompt += "  - \"yeah do it\" / \"fire it up\" / \"ship it\" / \"send it\"\n"
			systemPrompt += "  - \"yes\" or \"sure\" when you've already brought up the install\n\n"
			systemPrompt += "Trust your read of the conversation. If it sounds like a yes, it's a yes.\n\n"
			systemPrompt += "DO NOT call install_update for:\n\n"
			systemPrompt += "  - questions about what changed (\"what's in this release?\")\n"
			systemPrompt += "  - status checks (\"is there an update?\", \"do we need one?\")\n"
			systemPrompt += "  - speculation (\"should I update?\", \"is it worth it?\")\n"
			systemPrompt += "  - hesitation (\"maybe later\", \"idk\")\n\n"
			systemPrompt += "If you're genuinely uncertain whether they're asking, reply with one short clarifying question (\"Confirm: install " + p.Tag + " now, sir?\") and DON'T call the tool. The user will need to address you again with their answer.\n\n"
			systemPrompt += "When you DO call the tool, phrase your reply as though you're personally seeing the install through (\"Initiating install of " + p.Tag + ", sir. Stand by.\"). Stay in your voice."
		} else {
			systemPrompt += "(no tools available right now — there's no pending update.)\n\nIf the user asks you to install something, explain politely that there's nothing to install — Otto is on the latest version."
		}
	}

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
		log.Printf("toot reply error: %v (falling back to static)", err)
		if dErr := t.deliver(ctx, chatID, "Apologies, sir. Briefly indisposed. Try me again in a moment."); dErr != nil {
			log.Printf("toot reply fallback deliver error: %v", dErr)
		}
		return
	}

	out := strings.TrimSpace(assistantText.String())
	out, shouldTrigger := stripUpdateMarker(out)
	if out == "" {
		out = "Noted."
	}
	out = stripMarkdown(out)
	if dErr := t.deliver(ctx, chatID, out); dErr != nil {
		log.Printf("toot reply deliver error: %v", dErr)
	}

	// Fire the install AFTER the deliver so the user sees Toot's
	// "initiating install" message before the binary swap kicks off.
	// runUpdate handles the rest (download → swap → Confirm → Exit).
	if shouldTrigger && t.triggerUpdate != nil {
		log.Printf("toot: user-authorized install via chat marker")
		go t.triggerUpdate()
	}
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

	if err != nil {
		// Fall back to a static announcement so the user still hears
		// about the release if Claude is briefly unavailable.
		log.Printf("toot announce error: %v (falling back to static)", err)
		fallback := fmt.Sprintf(
			"Release %s is available (current: %s). Reply /update to install.",
			newTag, currentVersion,
		)
		return t.deliver(ctx, chatID, fallback)
	}

	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		out = fmt.Sprintf("Release %s is available. Reply /update to install.", newTag)
	}
	out = stripMarkdown(out)
	return t.deliver(ctx, chatID, out)
}

// Confirm sends the post-install "restarting" message in Toot's voice.
// No LLM call — the message is short, predictable, and frequent
// enough that an extra Claude invocation per install would be waste.
// The voice is encoded in the templated string itself.
func (t *Toot) Confirm(ctx context.Context, chatID int64, tag string) error {
	body := fmt.Sprintf(
		"Installation complete. %s is in place. Restarting the process — Otto will be back online shortly.",
		tag,
	)
	return t.deliver(ctx, chatID, body)
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
