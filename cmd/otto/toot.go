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
	"time"

	"otto/internal/claude"
	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
	"otto/internal/telegram"
)

// tootModel pins Toot to Haiku. The prompt is small and the announcement
// task is light, so Haiku composes changelog summaries well at far less cost
// than Sonnet — matching the Haiku-first hot path.
const tootModel = "claude-haiku-4-5"

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
// session, persona — with a different prompt.
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

	mem      *memory.Core   // injected into Toot's prompt; nil disables
	store    *store.Store   // turn log; nil disables
	embedder embed.Embedder // embeds turns for semantic search; nil disables

	// version is the running otto binary's version string (set from main's
	// build-stamped var). Surfaced in Toot's chat prompt so he can answer
	// "what version are we on?" without telling the user to go check git.
	version string

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

	// checkNow, when non-nil, runs a synchronous GitHub release poll
	// and returns the latest pending update (nil if none). Toot calls
	// this when emitting [CHECK_FOR_UPDATE] in chat.
	checkNow func(ctx context.Context) *pendingUpdate

	mu         sync.Mutex // serializes Toot's own --resume against the toot session
	lastActive time.Time  // last reply time; drives idle session rotation (guarded by mu)
}

// tootUpdateMarker is the literal string Toot's LLM is instructed to
// emit when the user has authorized an install during chat. The
// dispatcher strips it from the visible reply and fires triggerUpdate.
const tootUpdateMarker = "[TRIGGER_UPDATE]"

// tootCheckMarker is the literal string Toot's LLM is instructed to
// emit when the user has asked him to poll GitHub right now. The
// dispatcher strips it from the visible reply, runs CheckNow, and
// appends a one-line result to Toot's outgoing message.
const tootCheckMarker = "[CHECK_FOR_UPDATE]"

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

// stripCheckMarker removes the check-for-update marker from Toot's
// reply. Returns the cleaned text and a bool indicating whether the
// marker was present. Mirrors stripUpdateMarker semantics so both
// markers can coexist on the same reply.
func stripCheckMarker(s string) (string, bool) {
	if !strings.Contains(s, tootCheckMarker) {
		return s, false
	}
	cleaned := strings.ReplaceAll(s, tootCheckMarker, "")
	return strings.TrimSpace(cleaned), true
}

// Name returns "toot" — used by the petRegistry to route messages
// addressed to him directly.
func (t *Toot) Name() string { return "toot" }

// rotateIfIdle clears Toot's session if it has gone idle for at least window,
// mirroring Otto's idle reset. Without this the pet session lives forever and
// can answer from stale history (e.g. an old version number). Holding mu means
// it can never race a live reply.
func (t *Toot) rotateIfIdle(window time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.session.ID() == "" || t.lastActive.IsZero() {
		return
	}
	if idle := time.Since(t.lastActive); idle >= window {
		if err := t.session.Clear(); err != nil {
			log.Printf("rotator: clear toot session: %v", err)
			return
		}
		log.Printf("rotator: cleared toot session (idle %s)", idle.Round(time.Second))
	}
}

// Reply runs a chat turn — the user addressed Toot directly. Stays in
// his nerdy/dutiful voice but engages conversationally rather than
// reciting changelog. Most tools remain disallowed; Toot can use his
// bus-messaging tools for relays but otherwise just talks.
func (t *Toot) Reply(ctx context.Context, chatID int64, userMessage string) {
	t.reply(ctx, chatID, userMessage, nil)
}

// BusReply runs a chat turn for a message arriving via the inbox from
// another agent. The per-call system prompt grows a BUS CONTEXT + HOPS
// REMAINING block so Toot can reply via message_<sender> or wind down on
// the last hop.
func (t *Toot) BusReply(ctx context.Context, chatID int64, body string, bc busContext) {
	t.reply(ctx, chatID, body, &bc)
}

// tootAllowedTools is the closed allowlist of MCP tools Toot may call.
// He gets the inbox bus tools so he can relay back to Toto / Otto and
// session_search so he can remember past releases, but nothing that
// touches the world directly. The scoped mcp.json restricts what's
// even reachable; this allowlist tightens it further.
var tootAllowedTools = []string{
	"mcp__otto-memory__message_toto",
	"mcp__otto-memory__forward_to_otto",
	"mcp__otto-memory__session_search",
}

// reply is the shared body of Reply / BusReply. bc is nil for direct
// chat turns (Telegram-addressed) and non-nil for inbox-dispatched
// turns, in which case the BUS CONTEXT block is prepended and the
// runner is wrapped with env vars carrying the hop counter + self-name.
func (t *Toot) reply(ctx context.Context, chatID int64, userMessage string, bc *busContext) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastActive = time.Now()

	// Build the per-call system prompt with a strings.Builder to avoid the
	// quadratic allocation behaviour of repeated += on a growing string.
	// Each += copies all accumulated bytes so far; with ~40 concatenations
	// and a ~3 KB result the savings are modest but the pattern is correct.
	var sp strings.Builder
	sp.Grow(4096)
	sp.WriteString(t.persona)
	if bc != nil {
		sp.WriteString("\n\n")
		sp.WriteString(busPromptBlock(*bc, "toot"))
	}
	sp.WriteString("\n\n───────────────────────────────────────────────\n")
	sp.WriteString("THE USER ADDRESSED YOU DIRECTLY (CHAT MODE).\n")
	sp.WriteString("───────────────────────────────────────────────\n\n")
	sp.WriteString("This is not a release announcement. They want to talk to YOU. Stay in your voice — dutiful, formal-ish, dryly nerdy — but engage. You may discuss Otto, Toto, releases, your job, whatever they bring up. Decline tool requests politely (you only talk). Keep replies brief; phone-screen friendly.")

	// Running version — so "what version are we on?" gets a real answer
	// instead of a "check git yourself" deflection.
	if t.version != "" {
		sp.WriteString("\n\n───────────────────────────────────────────────\n")
		sp.WriteString("CURRENT OTTO VERSION (this running build):\n")
		sp.WriteString("───────────────────────────────────────────────\n\n")
		sp.WriteString(t.version)
		sp.WriteString("\n\nIf asked what version is running, answer with this directly. If the user asks whether they're up to date, compare against the pending-update tag below (if any) or say there's no pending release.")
	}

	// Inject the install_update tool when a release is pending. Framing
	// it as a "tool you can call" (rather than a strict marker rule)
	// nudges Toot toward using judgment on natural phrasings ("can you
	// update", "would you mind installing", etc.) rather than only
	// matching the literal example wordings.
	if t.pendingUpdate != nil || t.checkNow != nil {
		sp.WriteString("\n\n───────────────────────────────────────────────\n")
		sp.WriteString("TOOLS AVAILABLE TO YOU\n")
		sp.WriteString("───────────────────────────────────────────────\n\n")
	}
	if t.pendingUpdate != nil {
		if p := t.pendingUpdate(); p != nil {
			fmt.Fprintf(&sp, "install_update — installs the pending release (%s).\n\n", p.Tag)
			sp.WriteString("To call this tool, end your reply with the literal marker on its own line:\n\n  ")
			sp.WriteString(tootUpdateMarker)
			sp.WriteString("\n\nThe marker is invisible to the user — the system strips it and starts the install. After install completes, the standard \"Installed v…, restarting\" confirmation appears.\n\n")
			sp.WriteString("WHEN TO CALL install_update\n\n")
			sp.WriteString("Use your judgment. If the user's message is *any reasonable form* of asking you to install — direct, polite, colloquial, terse — call the tool. Don't be overly literal:\n\n")
			sp.WriteString("  - \"do it\" / \"update\" / \"install\" / \"go ahead\"\n")
			sp.WriteString("  - \"can you update?\" / \"could you install?\" / \"would you mind?\"\n")
			sp.WriteString("  - \"hey toot can you update\" / \"toot please install\"\n")
			sp.WriteString("  - \"yeah do it\" / \"fire it up\" / \"ship it\" / \"send it\"\n")
			sp.WriteString("  - \"yes\" or \"sure\" when you've already brought up the install\n\n")
			sp.WriteString("Trust your read of the conversation. If it sounds like a yes, it's a yes.\n\n")
			sp.WriteString("DO NOT call install_update for:\n\n")
			sp.WriteString("  - questions about what changed (\"what's in this release?\")\n")
			sp.WriteString("  - status checks (\"is there an update?\", \"do we need one?\")\n")
			sp.WriteString("  - speculation (\"should I update?\", \"is it worth it?\")\n")
			sp.WriteString("  - hesitation (\"maybe later\", \"idk\")\n\n")
			fmt.Fprintf(&sp, "If you're genuinely uncertain whether they're asking, reply with one short clarifying question (\"Confirm: install %s now, sir?\") and DON'T call the tool. The user will need to address you again with their answer.\n\n", p.Tag)
			fmt.Fprintf(&sp, "When you DO call the tool, phrase your reply as though you're personally seeing the install through (\"Initiating install of %s, sir. Stand by.\"). Stay in your voice.\n\n", p.Tag)
		} else if t.checkNow == nil {
			// No pending update AND no on-demand check tool available — the
			// user has no install path to offer. Tell Toot to deflect.
			sp.WriteString("(no tools available right now — there's no pending update.)\n\nIf the user asks you to install something, explain politely that there's nothing to install — Otto is on the latest version.\n\n")
		}
	}
	if t.checkNow != nil {
		sp.WriteString("check_for_update — runs an immediate release poll right now.\n\n")
		sp.WriteString("To call this tool, end your reply with the literal marker on its own line:\n\n  ")
		sp.WriteString(tootCheckMarker)
		sp.WriteString("\n\nThe marker is invisible to the user — the system polls GitHub and appends a one-line result to your message (\"Update found: vX.Y.Z.\" or \"Up to date as of HH:MM.\").\n\n")
		sp.WriteString("WHEN TO CALL check_for_update\n\n")
		sp.WriteString("  - User asks any form of \"check for updates\", \"is there a new release\", \"anything new on github\", \"see if there's a patch\".\n")
		sp.WriteString("  - User pings you out of curiosity (\"yo what's the latest\", \"any new version?\") — call it.\n\n")
		sp.WriteString("If the user ALSO wants you to install whatever comes back (\"check and install\", \"if there's one, do it\", \"yes and update if found\"), end your reply with BOTH markers, each on its own line:\n\n  ")
		sp.WriteString(tootCheckMarker)
		sp.WriteString("\n  ")
		sp.WriteString(tootUpdateMarker)
		fmt.Fprintf(&sp, "\n\nIf the check returns nothing, %s safely no-ops.", tootUpdateMarker)
	}

	systemPrompt := composePromptWithTimeAndMemory(sp.String(), t.mem)

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

	runner := t.runner
	if bc != nil {
		runner = runner.WithEnv(busEnv(bc.Hop, "toot"))
	}
	err := runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		SessionID:          t.session.ID(),
		Model:              tootModel,
		Effort:             tootEffort,
		AllowedTools:       tootAllowedTools,
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
	out, shouldCheck := stripCheckMarker(out)
	out, shouldTrigger := stripUpdateMarker(out)
	if out == "" {
		if shouldCheck {
			out = "Checking, sir."
		} else {
			out = "Noted."
		}
	}

	// Run the synchronous release poll BEFORE delivering so the result
	// line can ride along on the same message. Bounded context keeps a
	// slow GitHub from hanging Toot's reply chain.
	var checkResult *pendingUpdate
	if shouldCheck && t.checkNow != nil {
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		checkResult = t.checkNow(checkCtx)
		cancel()
		if checkResult != nil {
			out += "\n\nUpdate found: " + checkResult.Tag + "."
		} else {
			out += "\n\nUp to date as of " + time.Now().Format("15:04") + "."
		}
	}

	out = stripMarkdown(out)
	if dErr := t.deliver(ctx, chatID, out); dErr != nil {
		log.Printf("toot reply deliver error: %v", dErr)
	}

	logTurn(ctx, t.store, t.embedder, "toot", "user", userMessage)
	logTurn(ctx, t.store, t.embedder, "toot", "assistant", out)

	// Fire the install AFTER the deliver so the user sees Toot's
	// "initiating install" message before the binary swap kicks off.
	// runUpdate handles the rest (download → swap → Confirm → Exit).
	//
	// Combined check+install: only fire if the just-completed CheckNow
	// turned up a release. Otherwise log and skip — there's nothing to
	// install and Pending() is already nil so /update would error too.
	if shouldTrigger && t.triggerUpdate != nil {
		if shouldCheck && checkResult == nil {
			log.Printf("toot: check+install requested but no update — skipping install")
		} else {
			log.Printf("toot: user-authorized install via chat marker")
			go t.triggerUpdate()
		}
	}
}

// Announce composes a release notification in Toot's voice and sends
// it. body is the GitHub release notes (auto-generated changelog when
// generate_release_notes: true). Toot reads them, explains the items,
// and signs off in character.
func (t *Toot) Announce(ctx context.Context, chatID int64, currentVersion, newTag, body string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastActive = time.Now()

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

	// Surface current local/UTC time so Toot can place the release in the
	// user's day ("evening release", "early morning push", etc.) and answer
	// follow-up questions about timing accurately.
	systemPrompt = composePromptWithTimeAndMemory(systemPrompt, nil)

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
//
// The phrasing primes the user for a brief outage (~10s, capped by the
// updater's force-exit fallback) and promises a follow-up ping from the
// next boot — see maybeSendBootConfirm in main.go.
func (t *Toot) Confirm(ctx context.Context, chatID int64, tag string) error {
	body := fmt.Sprintf(
		"Installation complete. %s is in place. Restarting now — expect ~10s downtime, I'll ping back when we're up.",
		tag,
	)
	return t.deliver(ctx, chatID, body)
}

// SystemMessage delivers a templated message in Toot's voice with no
// LLM call. Used for boot-back-online pings and similar lifecycle
// notifications where the prose is fixed at the call site.
func (t *Toot) SystemMessage(ctx context.Context, chatID int64, body string) error {
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
