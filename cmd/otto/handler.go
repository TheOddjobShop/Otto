//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/permissions"
	"otto/internal/telegram"
)

const callbackPrefixPerm = "perm:"

const (
	pollErrorBaseBackoff = time.Second
	pollErrorMaxBackoff  = time.Minute
	claudeCallTimeout    = 5 * time.Minute
)

type handler struct {
	bot           telegram.BotClient
	allow         *auth.Allowlist
	session       *claude.Session
	runner        claude.Runner
	pending       *permissions.Pending
	settingsPath  string // ~/.claude/settings.json (or override)
	startedAt     time.Time

	mu sync.Mutex // serializes Claude calls per design spec
}

func (h *handler) runPollingLoop(ctx context.Context) error {
	offset := 0
	backoff := pollErrorBaseBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := h.bot.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("polling error: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > pollErrorMaxBackoff {
				backoff = pollErrorMaxBackoff
			}
			continue
		}
		backoff = pollErrorBaseBackoff
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			h.dispatch(ctx, u)
		}
	}
}

func (h *handler) dispatch(ctx context.Context, u telegram.Update) {
	if !h.allow.Allows(u.UserID) {
		log.Printf("dropping message from non-allowlisted user %d", u.UserID)
		return
	}
	if u.IsCallback() {
		h.handleCallback(ctx, u)
		return
	}
	if strings.TrimSpace(u.Text) == "" && len(u.PhotoIDs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cmd := h.tryCommand(ctx, u); cmd.handled {
		if err := telegram.SendChunked(ctx, h.bot, u.ChatID, cmd.reply); err != nil {
			log.Printf("send error (command reply): %v", err)
		}
		return
	}
	h.handleMessage(ctx, u)
}

// handleCallback processes an inline-keyboard button tap. Currently only
// "perm:<id>:<once|always|deny>" callbacks (from the permission-denial flow)
// are recognized; anything else just dismisses the loading spinner.
//
// once  — replay the original prompt with --allowed-tools <pattern>; no
//         persistent settings.json change.
// always — write the pattern into ~/.claude/settings.json's permissions.allow,
//         then replay (also with --allowed-tools as belt-and-suspenders so
//         we don't depend on claude re-reading settings.json mid-stream).
// deny   — silent acknowledgement.
func (h *handler) handleCallback(ctx context.Context, u telegram.Update) {
	if !strings.HasPrefix(u.CallbackData, callbackPrefixPerm) {
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "")
		return
	}
	rest := strings.TrimPrefix(u.CallbackData, callbackPrefixPerm)
	parts := strings.SplitN(rest, ":", 2)
	if len(parts) != 2 {
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Bad callback")
		return
	}
	id, action := parts[0], parts[1]
	entry, ok := h.pending.Take(id)
	if !ok {
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Already handled or expired")
		return
	}
	switch action {
	case "once", "always":
		if action == "always" {
			if err := permissions.AllowTool(h.settingsPath, entry.Pattern); err != nil {
				log.Printf("permissions: allow %q: %v", entry.Pattern, err)
				_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Failed to write settings")
				_ = h.bot.SendMessage(ctx, u.ChatID, fmt.Sprintf("⚠️ Could not save permission: %v", err))
				return
			}
		}
		// Acknowledge before the long-running replay (Telegram dismisses
		// the button's loading spinner once we answer).
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Replaying…")

		// Take the same per-Claude-call lock as a regular message would, so
		// any user message that arrives while we're replaying queues behind
		// us instead of interleaving --resume against the same session.
		h.mu.Lock()
		defer h.mu.Unlock()

		callCtx, cancel := context.WithTimeout(ctx, claudeCallTimeout)
		defer cancel()
		h.runAndReply(callCtx, ctx, entry.ChatID, claude.RunArgs{
			Prompt:       entry.Prompt,
			SessionID:    h.session.ID(),
			AllowedTools: []string{entry.Pattern},
		})
	case "deny":
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Skipped")
	default:
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Unknown action")
	}
}

// surfaceDenials sends one inline-keyboard message per denied tool. Called
// after each Claude turn — denials are typically empty (the skip-permissions
// flag works), but when something slips through we want a tappable approval
// rather than a wall of text telling the user to edit settings.json.
//
// originalPrompt is captured into the pending entry so a tap on Allow can
// auto-replay without making the user re-send. Image attachments are not
// preserved (their tempdir is cleaned up when the originating handleMessage
// returns); image-message replays would need to re-download from Telegram.
func (h *handler) surfaceDenials(ctx context.Context, chatID int64, originalPrompt string, denials []claude.PermissionDenial) {
	seen := map[string]struct{}{}
	for _, d := range denials {
		pattern := permissions.PatternFor(d.ToolName)
		// One prompt per pattern (collapsing multiple denials of the same
		// MCP server family into a single button group).
		if _, dup := seen[pattern]; dup {
			continue
		}
		seen[pattern] = struct{}{}
		id := h.pending.Add(permissions.Entry{
			ToolName:  d.ToolName,
			Pattern:   pattern,
			ChatID:    chatID,
			Prompt:    originalPrompt,
			SessionID: h.session.ID(),
		})
		buttons := [][]telegram.InlineButton{{
			{Text: "✅ Once", CallbackData: callbackPrefixPerm + id + ":once"},
			{Text: "✅ Always", CallbackData: callbackPrefixPerm + id + ":always"},
			{Text: "❌ Skip", CallbackData: callbackPrefixPerm + id + ":deny"},
		}}
		text := fmt.Sprintf("⚠️ Claude tried `%s` and was denied.\n\n• *Once* — allow this retry only\n• *Always* — save `%s` to settings and retry\n• *Skip* — do nothing",
			d.ToolName, pattern)
		if err := h.bot.SendMessageWithButtons(ctx, chatID, text, buttons); err != nil {
			log.Printf("send error (denial buttons): %v", err)
		}
	}
}

func (h *handler) handleMessage(ctx context.Context, u telegram.Update) {
	callCtx, cancel := context.WithTimeout(ctx, claudeCallTimeout)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "otto-photos-")
	if err != nil {
		if sendErr := telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ tempdir: %v", err)); sendErr != nil {
			log.Printf("send error (tempdir failure): %v", sendErr)
		}
		return
	}
	defer os.RemoveAll(tmpDir)

	var imagePaths []string
	for _, pid := range u.PhotoIDs {
		path, err := telegram.DownloadPhotoToTemp(callCtx, h.bot, pid, tmpDir)
		if err != nil {
			if sendErr := telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ photo download: %v", err)); sendErr != nil {
				log.Printf("send error (photo download failure): %v", sendErr)
			}
			return
		}
		imagePaths = append(imagePaths, path)
	}

	h.runAndReply(callCtx, ctx, u.ChatID, claude.RunArgs{
		Prompt:     u.Text,
		SessionID:  h.session.ID(),
		ImagePaths: imagePaths,
	})
}

// runAndReply runs claude with args, drains the event stream, captures the
// session ID, sends replies under sendCtx (parent ctx — so the reply isn't
// cancelled by the 5-minute Claude timeout), and surfaces any permission
// denials as inline-keyboard buttons. Shared between handleMessage and the
// permission-button replay path so both paths handle errors, ResultEvent
// failures, and follow-up denials identically.
//
// callCtx governs the Claude subprocess (timeout); sendCtx governs Telegram
// replies (parent, longer-lived).
func (h *handler) runAndReply(callCtx, sendCtx context.Context, chatID int64, args claude.RunArgs) {
	events := make(chan claude.Event, 64)
	args.Events = events

	doneParsing := make(chan struct{})
	var assistantText strings.Builder
	var lastResult claude.ResultEvent
	var capturedSessionID string

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
			}
		}
	}()

	err := h.runner.Run(callCtx, args)
	close(events)
	<-doneParsing

	// Persist whatever session ID Claude Code reported, even on error —
	// often the system/init event arrives before a downstream failure, and
	// we want subsequent messages to resume the same conversation rather
	// than start a fresh one.
	if capturedSessionID != "" && capturedSessionID != h.session.ID() {
		if setErr := h.session.Set(capturedSessionID); setErr != nil {
			log.Printf("session save: %v", setErr)
		}
	}

	if err != nil {
		if sendErr := telegram.SendChunked(sendCtx, h.bot, chatID, fmt.Sprintf("⚠️ Claude error: %s", err)); sendErr != nil {
			log.Printf("send error (claude failure): %v", sendErr)
		}
		return
	}

	if lastResult.Subtype != "" && lastResult.Subtype != "success" {
		msg := fmt.Sprintf("⚠️ Claude result %s", lastResult.Subtype)
		if lastResult.Error != "" {
			msg += ": " + lastResult.Error
		}
		if sendErr := telegram.SendChunked(sendCtx, h.bot, chatID, msg); sendErr != nil {
			log.Printf("send error (result failure): %v", sendErr)
		}
		return
	}

	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		out = "(no response)"
	}
	// Belt-and-suspenders: the system prompt says no markdown, but claude
	// slips. Telegram doesn't render markdown without parse_mode, so any
	// leftover markup would appear literally. Strip it.
	out = stripMarkdown(out)
	if err := telegram.SendChunked(sendCtx, h.bot, chatID, out); err != nil {
		log.Printf("send error: %v", err)
	}
	if len(lastResult.PermissionDenials) > 0 {
		h.surfaceDenials(sendCtx, chatID, args.Prompt, lastResult.PermissionDenials)
	}
}
