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
	"otto/internal/telegram"
)

const (
	pollErrorBaseBackoff = time.Second
	pollErrorMaxBackoff  = time.Minute
)

type handler struct {
	bot       telegram.BotClient
	allow     *auth.Allowlist
	session   *claude.Session
	runner    claude.Runner
	startedAt time.Time

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
// non-blocking checks (so a fresh Telegram message can route to Toto via
// the dispatch busy-detect handoff, without waiting).
type ottoState struct {
	mu            sync.Mutex
	busy          bool
	currentPrompt string
	cancel        context.CancelFunc
	lastEvent     time.Time
	suppressError bool
	// lastSnippet is the tail of Otto's in-flight assistant text, capped
	// to snippetCap bytes. Surfaced to Toto so that during Otto's busy
	// window Toto can ground replies in what Otto is actually saying
	// right now ("he's typing about your gmail, hold on") instead of
	// just "he's busy."
	lastSnippet string
}

// snippetCap bounds how many tail bytes of Otto's stream we expose to
// Toto. ~600 leaves room for a sentence or two, enough to be useful as
// progress context without blowing up Toto's haiku prompt.
const snippetCap = 600

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

func (s *ottoState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	s.currentPrompt = ""
	s.lastSnippet = ""
	s.cancel = nil
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

// appendSnippet adds streamed assistant text to the tail buffer, trimming
// from the front when it grows past snippetCap. Concurrency: called from
// the runAndReply event-consumer goroutine; protected by mu so the Toto
// dispatch path's snapshot read is race-free.
func (s *ottoState) appendSnippet(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSnippet += text
	if len(s.lastSnippet) > snippetCap {
		s.lastSnippet = "…" + s.lastSnippet[len(s.lastSnippet)-snippetCap:]
	}
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
	// Toto path: capture Otto's in-flight prompt and the tail of his
	// streamed reply so Toto can refer to both.
	h.otto.mu.Lock()
	ottoPrompt := h.otto.currentPrompt
	ottoSnippet := h.otto.lastSnippet
	silence := time.Since(h.otto.lastEvent)
	h.otto.mu.Unlock()
	previewIn := truncate(u.Text, 60)
	previewOut := truncate(ottoPrompt, 60)
	log.Printf("otto busy → toto (silence=%s) msg=%q inflight=%q", silence.Round(time.Second), previewIn, previewOut)
	h.toto.Reply(ctx, u.ChatID, u.Text, ottoPrompt, ottoSnippet)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// surfaceDenials sends one plain-text message per unique denied-tool pattern
// with copy-pasteable instructions for editing ~/.claude/settings.json.
// Called after each Claude turn — denials are typically empty (the
// skip-permissions flag works), but when something slips through we want
// the user to know what to add and where.
func (h *handler) surfaceDenials(ctx context.Context, chatID int64, _ string, denials []claude.PermissionDenial) {
	seen := map[string]struct{}{}
	for _, d := range denials {
		pattern := patternForTool(d.ToolName)
		if _, dup := seen[pattern]; dup {
			continue
		}
		seen[pattern] = struct{}{}
		text := fmt.Sprintf(
			"⚠️ Claude tried to use %s and was denied.\n\nTo allow it next time, add %s to permissions.allow in ~/.claude/settings.json, then /restart.",
			d.ToolName, pattern,
		)
		if err := telegram.SendChunked(ctx, h.bot, chatID, text); err != nil {
			log.Printf("send error (denial text): %v", err)
		}
	}
}

// patternForTool turns a tool name into a permission pattern suitable for
// settings.json's permissions.allow array. MCP tools become a wildcard
// over the whole server family; built-in tool names are returned verbatim.
func patternForTool(toolName string) string {
	if strings.HasPrefix(toolName, "mcp__") {
		rest := strings.TrimPrefix(toolName, "mcp__")
		if i := strings.LastIndex(rest, "__"); i > 0 {
			return "mcp__" + rest[:i] + "__*"
		}
	}
	return toolName
}

// handleMessage runs an Otto turn. Caller must have already acquired the
// Otto slot via h.otto.tryAcquire and is responsible for calling
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

// runAndReply drives a Claude subprocess: it streams args.Events, parses
// assistant text / session ID / result events, sends the assistant reply
// over Telegram, and surfaces any permission denials as plain-text
// instructions for editing settings.json.
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
				h.otto.appendSnippet(e.Text)
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
