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
)

type handler struct {
	bot          telegram.BotClient
	allow        *auth.Allowlist
	session      *claude.Session
	runner       claude.Runner
	pending      *permissions.Pending
	settingsPath string
	startedAt    time.Time

	otto *ottoState
	toto *Toto

	// dispatchWG tracks in-flight dispatch goroutines so the polling
	// loop's caller (main.go on shutdown, or tests after their window)
	// can wait for them to drain instead of returning while goroutines
	// still hold the Otto slot.
	dispatchWG sync.WaitGroup
}

// WaitDispatches blocks until all dispatch goroutines spawned by the
// polling loop have returned. Call after runPollingLoop returns to ensure
// in-flight Telegram messages finish processing.
func (h *handler) WaitDispatches() { h.dispatchWG.Wait() }

// ottoState gates concurrent access to the single Otto subprocess slot and
// holds metadata about the in-flight call (used by the watchdog to detect
// hangs and by Toto to give context-aware replies while Otto is busy).
//
// "Busy" is a single boolean under mu, not a sync.Mutex, because we need
// non-blocking checks (so a fresh Telegram message can route to Toto
// without waiting) and blocking waits (so a permission-button replay
// queues behind any in-flight call). A waiter list signaled on release
// supports the blocking case.
type ottoState struct {
	mu            sync.Mutex
	busy          bool
	waiters       []chan struct{}
	currentPrompt string
	cancel        context.CancelFunc
	lastEvent     time.Time
	suppressError bool
}

func newOttoState() *ottoState {
	return &ottoState{}
}

// tryAcquire is non-blocking. Returns true if Otto was free and is now
// claimed; false if Otto was already busy.
func (s *ottoState) tryAcquire(prompt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	s.currentPrompt = prompt
	s.lastEvent = time.Now()
	s.suppressError = false
	return true
}

// acquire blocks until Otto is free or ctx is cancelled. Used by the
// permission-button replay path so that taps queue behind an in-flight
// Otto call (preserving the original h.mu serialization semantics).
func (s *ottoState) acquire(ctx context.Context, prompt string) error {
	for {
		s.mu.Lock()
		if !s.busy {
			s.busy = true
			s.currentPrompt = prompt
			s.lastEvent = time.Now()
			s.suppressError = false
			s.mu.Unlock()
			return nil
		}
		wait := make(chan struct{})
		s.waiters = append(s.waiters, wait)
		s.mu.Unlock()
		select {
		case <-wait:
			// Re-check — another waiter may have grabbed the slot first.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *ottoState) release() {
	s.mu.Lock()
	s.busy = false
	s.currentPrompt = ""
	s.cancel = nil
	waiters := s.waiters
	s.waiters = nil
	s.mu.Unlock()
	for _, w := range waiters {
		close(w)
	}
}

func (s *ottoState) setCancel(c context.CancelFunc) {
	s.mu.Lock()
	s.cancel = c
	s.mu.Unlock()
}

func (s *ottoState) markEvent() {
	s.mu.Lock()
	s.lastEvent = time.Now()
	s.mu.Unlock()
}

func (s *ottoState) markSuppressError() {
	s.mu.Lock()
	s.suppressError = true
	s.mu.Unlock()
}

// shouldSuppressError reports whether the last cancellation came from the
// watchdog (so the resulting context.Canceled error from runner.Run should
// not be surfaced to the user as a Claude error — Toto already messaged
// them about the reboot).
func (s *ottoState) shouldSuppressError() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.suppressError
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
			// Async dispatch so Otto's long-running call doesn't block
			// the polling loop or Toto's fallback replies.
			h.dispatchWG.Add(1)
			go func(u telegram.Update) {
				defer h.dispatchWG.Done()
				h.dispatch(ctx, u)
			}(u)
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
	// Commands are read-only or session-only — they don't acquire the Otto
	// slot, so /whoami / /status etc. work even while Otto is busy.
	if cmd := h.tryCommand(ctx, u); cmd.handled {
		if err := telegram.SendChunked(ctx, h.bot, u.ChatID, cmd.reply); err != nil {
			log.Printf("send error (command reply): %v", err)
		}
		return
	}
	// Try to claim Otto. If he's free, run him; if he's busy, hand off to
	// Toto so the user gets a reply instead of silence.
	if h.otto.tryAcquire(u.Text) {
		defer h.otto.release()
		h.handleMessage(ctx, u)
		return
	}
	// Toto path: capture Otto's in-flight prompt so Toto can refer to it.
	h.otto.mu.Lock()
	ottoPrompt := h.otto.currentPrompt
	h.otto.mu.Unlock()
	h.toto.Reply(ctx, u.ChatID, u.Text, ottoPrompt)
}

// handleCallback processes an inline-keyboard button tap. Currently only
// "perm:<id>:<once|always|deny>" callbacks (from the permission-denial flow)
// are recognized; anything else just dismisses the loading spinner.
//
// Replays acquire the Otto slot via blocking acquire — taps that arrive
// while Otto is mid-call queue behind it (preserving the pre-Toto
// serialization semantics for permission button taps specifically).
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
		_ = h.bot.AnswerCallbackQuery(ctx, u.CallbackQueryID, "Replaying…")
		if err := h.otto.acquire(ctx, entry.Prompt); err != nil {
			log.Printf("callback acquire: %v", err)
			return
		}
		defer h.otto.release()

		callCtx, cancel := context.WithCancel(ctx)
		h.otto.setCancel(cancel)
		defer cancel()

		done := make(chan struct{})
		defer close(done)
		go h.runWatchdog(ctx, entry.ChatID, done)

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

// handleMessage runs an Otto turn. Caller must have already acquired the
// Otto slot via h.otto.tryAcquire / acquire and is responsible for calling
// h.otto.release.
func (h *handler) handleMessage(ctx context.Context, u telegram.Update) {
	callCtx, cancel := context.WithCancel(ctx)
	h.otto.setCancel(cancel)
	defer cancel()

	watchdogDone := make(chan struct{})
	defer close(watchdogDone)
	go h.runWatchdog(ctx, u.ChatID, watchdogDone)

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
		path, err := telegram.DownloadPhotoToTemp(ctx, h.bot, pid, tmpDir)
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
// session ID, sends replies, and surfaces any permission denials as inline-
// keyboard buttons. Shared between handleMessage and the permission-button
// replay path so both paths handle errors, ResultEvent failures, and follow-
// up denials identically.
//
// Side effect: every event consumed bumps h.otto.lastEvent, which the
// watchdog uses to detect hangs. If callCtx was cancelled by the watchdog,
// h.otto.suppressError is set, and we drop the resulting "context canceled"
// error rather than echoing it as a Claude error (Toto already informed
// the user about the reboot).
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
			h.otto.markEvent()
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

	if capturedSessionID != "" && capturedSessionID != h.session.ID() {
		if setErr := h.session.Set(capturedSessionID); setErr != nil {
			log.Printf("session save: %v", setErr)
		}
	}

	if err != nil {
		if h.otto.shouldSuppressError() {
			// Watchdog already messaged the user about the reboot.
			return
		}
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
	out = stripMarkdown(out)
	if err := telegram.SendChunked(sendCtx, h.bot, chatID, out); err != nil {
		log.Printf("send error: %v", err)
	}
	if len(lastResult.PermissionDenials) > 0 {
		h.surfaceDenials(sendCtx, chatID, args.Prompt, lastResult.PermissionDenials)
	}
}
