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
	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
	"otto/internal/telegram"
)

const (
	pollErrorBaseBackoff = time.Second
	pollErrorMaxBackoff  = time.Minute
	// dispatchBatchSpacing is how long the polling loop pauses between
	// consecutive updates inside the same batch. Eliminates the race
	// where two parallel goroutines arrive at Otto's tryAcquire and
	// Toto's snapshot in opposite order, making Toto report "idle"
	// while Otto is just about to start working on a sibling message.
	dispatchBatchSpacing = 150 * time.Millisecond
)

type handler struct {
	bot       telegram.BotClient
	allow     *auth.Allowlist
	session   *claude.Session
	runner    claude.Runner
	startedAt time.Time

	otto    *ottoState
	toto    *Toto
	updater *updater
	pets    *petRegistry // routes name-addressed messages to Toto/Toot/etc.

	// classifier picks Otto's per-turn model (Sonnet for chat, Opus for
	// coding). Nil disables routing — Otto then inherits Claude Code's
	// default model. Set in production by main; left nil by tests that
	// don't exercise routing.
	classifier modelClassifier

	// petRotators are the pets (Toto/Toot) whose sessions the rotator clears
	// on the idle window, so they don't accumulate stale conversation state.
	petRotators []petRotator

	rotate rotateConfig // session-rotation thresholds (zero value disables)

	mem              *memory.Core   // injected into every Otto prompt; nil disables
	store            *store.Store   // turn log for session_search; nil disables
	embedder         embed.Embedder // embeds turns for semantic search; nil disables
	baseSystemPrompt string         // Otto's persona+footer prompt, before memory

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
	// gen increments on every slot acquisition. Cancellation paths (the
	// watchdog, /restart) snapshot it together with busy under mu and pass
	// it back to cancelInflight, which no-ops if the observed turn has
	// since ended — so a stale snapshot can never cancel a NEWER turn that
	// grabbed the slot in the meantime.
	gen uint64
	// lastSnippet is the tail of Otto's in-flight assistant text, capped
	// to snippetCap bytes. Surfaced to Toto so that during Otto's busy
	// window Toto can ground replies in what Otto is actually saying
	// right now ("he's typing about your gmail, hold on") instead of
	// just "he's busy."
	lastSnippet string

	lastInputTokens int       // ContextTokens (input+cache) of the most recent Otto turn
	lastUserMsg     time.Time // time of the most recent user message (idle calc)
	lastModel       string    // model id chosen for the most recent Otto turn ("" = inherited)

	// turnKey identifies the in-flight turn. Set on acquisition, it groups
	// activity rows so a reader can ask for "this turn" rather than "the last
	// N rows", which would interleave a bus turn with a Telegram one.
	turnKey string
	// activity is a bounded ring of the in-flight turn's tool calls, kept in
	// memory so Toto's dispatch path can read it without a DB round-trip.
	// Cleared on release alongside lastSnippet.
	activity []activityEntry
	// pendingTools maps tool_use id → tool name, so a tool_result (which
	// carries only the id) can be attributed to the tool that produced it.
	// Bounded by the same cap as the ring; a turn calling more tools than that
	// simply loses attribution on the oldest, which only affects a log line.
	pendingTools map[string]string
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
	s.gen++
	s.currentPrompt = prompt
	s.lastEvent = time.Now()
	s.suppressError = false
	s.startTurnLocked()
	return true
}

// startTurnLocked stamps a fresh turn key and clears the per-turn activity
// state. Caller must hold mu.
func (s *ottoState) startTurnLocked() {
	s.turnKey = newTurnKey()
	s.activity = nil
	s.pendingTools = map[string]string{}
}

// tryAcquireOrSnapshot is the dispatch-path atomic version of tryAcquire:
// either claim Otto, or return a consistent snapshot of his in-flight
// state for Toto to use as fallback context. Combining acquire and
// snapshot under a single critical section eliminates the window where
// Otto could finish between the failed tryAcquire and a follow-up read,
// which would have surfaced empty/zero context to Toto.
func (s *ottoState) tryAcquireOrSnapshot(prompt string) (acquired bool, snap ottoSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.busy {
		s.busy = true
		s.gen++
		s.currentPrompt = prompt
		s.lastEvent = time.Now()
		s.suppressError = false
		s.startTurnLocked()
		return true, ottoSnapshot{}
	}
	snap = ottoSnapshot{
		Busy:          true,
		CurrentPrompt: s.currentPrompt,
		Snippet:       s.lastSnippet,
		Activity:      append([]activityEntry(nil), s.activity...),
	}
	if !s.lastEvent.IsZero() {
		snap.Silence = time.Since(s.lastEvent)
	}
	return false, snap
}

func (s *ottoState) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.busy = false
	s.currentPrompt = ""
	s.lastSnippet = ""
	s.cancel = nil
	// Per-turn activity dies with the turn: the ring answers "what is Otto
	// doing RIGHT NOW", and reporting a finished turn's tool calls as current
	// would be exactly the kind of stale answer this feature exists to fix.
	// The durable copy lives in the activity table.
	s.activity = nil
	s.pendingTools = nil
	s.turnKey = ""
}

// pushActivityLocked appends to the ring, trimming from the front past the
// cap. Caller must hold mu.
func (s *ottoState) pushActivityLocked(e activityEntry) {
	s.activity = append(s.activity, e)
	if len(s.activity) > activityRingCap {
		// Re-slice from the tail into a fresh backing array. Slicing in place
		// would keep the whole original array alive for the life of the turn.
		s.activity = append([]activityEntry(nil), s.activity[len(s.activity)-activityRingCap:]...)
	}
}

// recordToolUse notes a tool invocation and returns the turn key it belongs
// to, or "" if no turn is in flight (in which case the caller skips the
// durable write too — an activity row with no turn is unreadable).
func (s *ottoState) recordToolUse(id, name, detail string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnKey == "" {
		return ""
	}
	if s.pendingTools == nil {
		s.pendingTools = map[string]string{}
	}
	if id != "" {
		// Bound the correlation map the same way as the ring. Without this a
		// turn making hundreds of tool calls would grow it unboundedly for the
		// whole turn, purely to attribute log lines.
		if len(s.pendingTools) >= activityRingCap*2 {
			s.pendingTools = map[string]string{}
		}
		s.pendingTools[id] = name
	}
	s.pushActivityLocked(activityEntry{
		At: time.Now(), Kind: store.ActivityTool, Tool: name, Detail: detail,
	})
	return s.turnKey
}

// recordToolResult correlates a result back to its tool by id and notes it.
// Returns the turn key and the resolved tool name; ok is false when no turn is
// in flight.
//
// Only failures enter the ring: a successful result adds a line that says
// nothing a reader would act on, and the ring is small enough that filling it
// with successes would push out the tool calls themselves.
func (s *ottoState) recordToolResult(id string, isError bool, content string) (turnKey, tool string, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnKey == "" {
		return "", "", false
	}
	tool = s.pendingTools[id]
	if id != "" {
		delete(s.pendingTools, id)
	}
	if isError {
		s.pushActivityLocked(activityEntry{
			At: time.Now(), Kind: store.ActivityResult, Tool: tool,
			Detail: content, IsError: true,
		})
	}
	return s.turnKey, tool, true
}

// currentTurnKey returns the in-flight turn's key, or "" when Otto is idle.
func (s *ottoState) currentTurnKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnKey
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

// setInputTokens records the session's latest observed input-token count.
func (s *ottoState) setInputTokens(n int) {
	s.mu.Lock()
	s.lastInputTokens = n
	s.mu.Unlock()
}

// resetInputTokens zeroes the token count (after a session clear/rotation).
func (s *ottoState) resetInputTokens() {
	s.mu.Lock()
	s.lastInputTokens = 0
	s.mu.Unlock()
}

// setModel records the model id chosen for the current Otto turn so /status
// can report which tier (Sonnet/Opus) the last message ran on.
func (s *ottoState) setModel(m string) {
	s.mu.Lock()
	s.lastModel = m
	s.mu.Unlock()
}

// markUserMessage records that the user just sent a message (resets idle).
func (s *ottoState) markUserMessage() {
	s.mu.Lock()
	s.lastUserMsg = time.Now()
	s.mu.Unlock()
}

// rotationSnapshot returns the latest token count and how long since the last
// user message. If no user message has been seen, idle is 0.
func (s *ottoState) rotationSnapshot() (tokens int, idle time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens = s.lastInputTokens
	if !s.lastUserMsg.IsZero() {
		idle = time.Since(s.lastUserMsg)
	}
	return tokens, idle
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
		tail := s.lastSnippet[len(s.lastSnippet)-snippetCap:]
		// The byte slice above can land inside a multi-byte UTF-8 sequence.
		// Advance past any leading continuation bytes (0x80–0xBF) so the
		// result is valid UTF-8 before it reaches Toto's system prompt or
		// the Claude API. ASCII and valid lead-bytes are unaffected.
		for len(tail) > 0 && tail[0]&0xC0 == 0x80 {
			tail = tail[1:]
		}
		s.lastSnippet = "…" + tail
	}
}

// cancelInflight atomically marks the next error as suppressed and cancels
// the in-flight Otto call — but only if the turn the caller observed (gen,
// captured under mu together with the busy snapshot) is still the one
// holding the slot. If that turn already finished — or a newer turn has
// since acquired the slot — it does nothing and returns false, so a stale
// watchdog/restart snapshot can never cancel (and silently swallow) a
// healthy newer turn. Holding mu across the check+suppress+cancel closes
// the TOCTOU window where release() could nil the cancel func between a
// separate read and the call. cancel() does not re-enter mu, so this
// cannot deadlock.
func (s *ottoState) cancelInflight(gen uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Require a live cancel func before claiming success. Between tryAcquire
	// (busy=true, gen++) and setCancel there is a sub-microsecond window where
	// busy && cancel==nil for the current turn. Without this guard, a /restart
	// or watchdog cancelInflight landing in that window would set suppressError
	// and return true without actually cancelling — falsely telling the user
	// the turn was interrupted and poisoning it so a later genuine error is
	// silently swallowed. Returning false here leaves the turn untouched; a
	// moment later setCancel registers the func and a retry works correctly.
	if !s.busy || s.gen != gen || s.cancel == nil {
		return false
	}
	s.suppressError = true
	s.cancel()
	return true
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

// ottoSnapshot is the immutable view of Otto's state that Toto
// receives so he can talk truthfully about what Otto's up to.
type ottoSnapshot struct {
	Busy          bool
	CurrentPrompt string        // Otto's in-flight prompt (only when Busy)
	Snippet       string        // tail of Otto's in-progress reply (only when Busy)
	Silence       time.Duration // how long since Otto's last stream event (zero if Otto has never run)
	// Activity is a copy of the in-flight turn's recent tool calls. Copied
	// rather than shared so the reader can never observe the slice mutating
	// under it after mu is released.
	Activity []activityEntry
}

// Snapshot returns the current Otto state under lock. Safe to call
// from any goroutine — used by Toto when answering "what's otto up to?"
// type questions.
func (s *ottoState) Snapshot() ottoSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := ottoSnapshot{
		Busy:          s.busy,
		CurrentPrompt: s.currentPrompt,
		Snippet:       s.lastSnippet,
		Activity:      append([]activityEntry(nil), s.activity...),
	}
	if !s.lastEvent.IsZero() {
		snap.Silence = time.Since(s.lastEvent)
	}
	return snap
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
			// Use time.NewTimer + Stop() instead of time.After so that the
			// internal timer goroutine is freed immediately when ctx.Done()
			// wins rather than leaking until the backoff fires (up to 1 min).
			backoffTimer := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				backoffTimer.Stop()
				return ctx.Err()
			case <-backoffTimer.C:
			}
			backoff *= 2
			if backoff > pollErrorMaxBackoff {
				backoff = pollErrorMaxBackoff
			}
			continue
		}
		backoff = pollErrorBaseBackoff
		// Reorder the batch so pet-addressed messages dispatch last. The
		// 150ms spacing below only fixes the busy-snapshot race when the
		// Otto-bound message happens to arrive first; Telegram's delivery
		// order is not guaranteed (network jitter can invert two messages
		// sent a second apart), and when it inverts, Toto snapshots an idle
		// Otto and tells the user "nothing's going on" while Otto is about
		// to start on their sibling message. Partitioning removes the
		// dependence on arrival order entirely.
		ordered := partitionPetLast(updates, h.pets)
		for i, u := range ordered {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			// When a batch contains multiple updates (user sent messages
			// close enough together to land in the same long-poll), pause
			// briefly between dispatches so each message's routing
			// decision lands in order. Combined with the pet-last
			// partition above, any Otto-bound sibling has claimed the slot
			// before a pet goroutine reads the snapshot. 150ms is plenty
			// for tryAcquire to win its mutex; single-message batches (the
			// common case) are unaffected.
			if i > 0 {
				// Same pattern as the backoff timer: NewTimer+Stop ensures no
				// goroutine leak when ctx.Done() wins, even at 150ms.
				spacingTimer := time.NewTimer(dispatchBatchSpacing)
				select {
				case <-spacingTimer.C:
				case <-ctx.Done():
					spacingTimer.Stop()
					return ctx.Err()
				}
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

// isPetAddressed reports whether u would route to a pet in dispatch. The
// classification mirrors dispatch's own pet check exactly — including the
// photo carve-out (photos always go to Otto regardless of caption text) —
// so the batch partition can never disagree with downstream routing.
func isPetAddressed(u telegram.Update, pets *petRegistry) bool {
	if pets == nil || len(u.PhotoIDs) > 0 {
		return false
	}
	_, _, ok := pets.Match(u.Text)
	return ok
}

// partitionPetLast returns updates reordered so that every non-pet-addressed
// update precedes every pet-addressed one, preserving the original relative
// order within each group.
//
// Reordering is benign even when the user genuinely typed the pet message
// first: the only updates affected are ones that landed in the same long-poll
// (typically sent under a second apart), where send-order intent is weak.
// Commands are treated as non-pet — they never acquire the Otto slot, so
// their position is irrelevant to the race, and dispatching them early keeps
// /status and friends snappy.
func partitionPetLast(updates []telegram.Update, pets *petRegistry) []telegram.Update {
	pet := make([]telegram.Update, 0, len(updates))
	ordered := make([]telegram.Update, 0, len(updates))
	for _, u := range updates {
		if isPetAddressed(u, pets) {
			pet = append(pet, u)
			continue
		}
		ordered = append(ordered, u)
	}
	return append(ordered, pet...)
}

func (h *handler) dispatch(ctx context.Context, u telegram.Update) {
	if !h.allow.Allows(u.UserID) {
		log.Printf("dropping message from non-allowlisted user %d", u.UserID)
		return
	}
	if strings.TrimSpace(u.Text) == "" && len(u.PhotoIDs) == 0 {
		return
	}
	h.otto.markUserMessage()
	// Commands are read-only or session-only — they don't acquire the Otto
	// slot, so /whoami / /status etc. work even while Otto is busy.
	if cmd := h.tryCommand(ctx, u); cmd.handled {
		if err := telegram.SendChunked(ctx, h.bot, u.ChatID, cmd.reply); err != nil {
			log.Printf("send error (command reply): %v", err)
		}
		return
	}
	// Pet routing: if the message is text-only (no photo) and addressed
	// to a known pet by name, route directly to the pet instead of Otto.
	// Photos always go to Otto — pets are pure-text in v1.
	if len(u.PhotoIDs) == 0 && h.pets != nil {
		if pet, body, ok := h.pets.Match(u.Text); ok {
			log.Printf("pet routing: %s ← %q", pet.Name(), truncate(u.Text, 60))
			pet.Reply(ctx, u.ChatID, body)
			return
		}
	}
	// Try to claim Otto. If he's free, run him; if he's busy, hand off to
	// Toto so the user gets a reply instead of silence. The snapshot is
	// captured atomically with the busy check so Toto's prompt reflects
	// Otto's in-flight state at the moment we lost the race for the slot.
	acquired, snap := h.otto.tryAcquireOrSnapshot(u.Text)
	if acquired {
		defer h.otto.release()
		h.handleMessage(ctx, u)
		return
	}
	previewIn := truncate(u.Text, 60)
	previewOut := truncate(snap.CurrentPrompt, 60)
	log.Printf("otto busy → toto (silence=%s) msg=%q inflight=%q", snap.Silence.Round(time.Second), previewIn, previewOut)
	// Guard against a nil Toto (possible in test configurations or partial
	// wiring). In production main.go always wires Toto before constructing
	// the handler, but omitting the check would panic on the first busy
	// message in any setup that leaves the field nil.
	if h.toto != nil {
		h.toto.BusyReply(ctx, u.ChatID, u.Text, snap.CurrentPrompt, snap.Snippet, snap.Activity)
	}
}

// truncate shortens s to at most n runes, appending "…" when it cuts.
// Rune-aware so that multi-byte characters (CJK, emoji, accented Latin)
// are never split mid-sequence, which would produce invalid UTF-8 in
// log lines, Toto's system prompt, and Telegram API payloads.
func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// surfaceDenials sends one plain-text message per unique denied-tool pattern
// with copy-pasteable instructions for editing ~/.claude/settings.json.
// Called after each Claude turn — denials are typically empty (the
// skip-permissions flag works), but when something slips through we want
// the user to know what to add and where.
func (h *handler) surfaceDenials(ctx context.Context, chatID int64, denials []claude.PermissionDenial) {
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

	// Only create the photo temp dir when the update actually carries
	// photos: text-only turns (the common case) must never fail — or pay
	// a create+remove syscall pair — for a directory they'd never use.
	var imagePaths []string
	if len(u.PhotoIDs) > 0 {
		tmpDir, err := os.MkdirTemp("", "otto-photos-")
		if err != nil {
			if sendErr := telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ tempdir: %v", err)); sendErr != nil {
				log.Printf("send error (tempdir failure): %v", sendErr)
			}
			return
		}
		defer os.RemoveAll(tmpDir)

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
	}

	// Route to a model: Sonnet for ordinary chat, Opus for coding tasks.
	// Empty (no classifier) means inherit Claude Code's default. The classify
	// call runs while Otto already holds the slot, so a concurrent message
	// still falls back to Toto rather than waiting on the router.
	model := ""
	if h.classifier != nil {
		model = h.classifier.classify(ctx, u.Text)
	}
	h.otto.setModel(model)

	h.runAndReply(callCtx, ctx, u.ChatID, claude.RunArgs{
		Prompt:             u.Text,
		SessionID:          h.session.ID(),
		ImagePaths:         imagePaths,
		Model:              model,
		Source:             "main",
		AppendSystemPrompt: composePromptWithTimeAndMemory(h.baseSystemPrompt, h.mem),
	}, h.runner)
}

// runAndReply drives a Claude subprocess: it streams args.Events, parses
// assistant text / session ID / result events, sends the assistant reply
// over Telegram, and surfaces any permission denials as plain-text
// instructions for editing settings.json.
//
// runner is the claude.Runner to use for this turn. The normal Telegram
// path passes h.runner; the agent-bus path passes a scoped runner (with
// hop-env vars added via WithEnv). Accepting it as an explicit parameter
// avoids mutating shared handler fields, which would require an implicit
// happens-before via the otto slot for safety rather than an explicit lock.
//
// Side effect: every event consumed bumps h.otto.lastEvent, which the
// watchdog uses to detect hangs. If callCtx was cancelled by the watchdog,
// h.otto.suppressError is set, and we drop the resulting "context canceled"
// error rather than echoing it as a Claude error (Toto already informed
// the user about the reboot).
func (h *handler) runAndReply(callCtx, sendCtx context.Context, chatID int64, args claude.RunArgs, runner claude.Runner) {
	events := make(chan claude.Event, 64)
	args.Events = events

	doneParsing := make(chan struct{})
	var assistantText strings.Builder
	var lastResult claude.ResultEvent
	var gotResult bool
	var capturedSessionID string

	// Bookend the turn in the activity log so a reader can tell a finished
	// turn from one still running, and see what it was asked to do.
	turnStart := time.Now()
	if tk := h.otto.currentTurnKey(); tk != "" {
		logActivity(sendCtx, h.store, store.ActivityEntry{
			Persona: "otto", TurnKey: tk, Kind: store.ActivityTurnStart,
			Detail: clip(oneLine(args.Prompt), activityDetailCap),
		})
	}

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
			case claude.ToolUseEvent:
				detail := summarizeToolInput(e.Name, e.Input)
				if tk := h.otto.recordToolUse(e.ID, e.Name, detail); tk != "" {
					logActivity(sendCtx, h.store, store.ActivityEntry{
						Persona: "otto", TurnKey: tk, Kind: store.ActivityTool,
						Tool: e.Name, Detail: detail,
					})
				}
			case claude.ToolResultEvent:
				tk, tool, ok := h.otto.recordToolResult(e.ToolUseID, e.IsError, clip(oneLine(e.Content), activityDetailCap))
				// Only failures are persisted, matching the ring: a row per
				// successful tool result would multiply the table's volume for
				// information no reader acts on.
				if ok && e.IsError {
					logActivity(sendCtx, h.store, store.ActivityEntry{
						Persona: "otto", TurnKey: tk, Kind: store.ActivityResult,
						Tool: tool, Detail: clip(oneLine(e.Content), activityDetailCap),
						IsError: true,
					})
				}
			case claude.ResultEvent:
				lastResult = e
				gotResult = true
			}
		}
	}()

	err := runner.Run(callCtx, args)
	close(events)
	<-doneParsing

	// Close the bookend. Recorded before the error/non-success early returns
	// below so a failed turn is still marked finished rather than looking
	// perpetually in-flight to a later reader.
	//
	// Deliberately NOT on sendCtx: that context is cancelled at shutdown, and
	// a turn interrupted by a restart is exactly the case where an unclosed
	// bookend is most misleading — the row would sit in the table forever
	// implying Otto is still working on something from three reboots ago.
	// WithoutCancel keeps the write while the timeout stops shutdown hanging
	// on a wedged database.
	if tk := h.otto.currentTurnKey(); tk != "" {
		endCtx, endCancel := context.WithTimeout(context.WithoutCancel(sendCtx), 5*time.Second)
		defer endCancel()
		outcome := fmt.Sprintf("ok in %s", time.Since(turnStart).Round(time.Second))
		isErr := false
		if err != nil {
			outcome = fmt.Sprintf("failed after %s: %v", time.Since(turnStart).Round(time.Second), err)
			isErr = true
		} else if lastResult.Subtype != "" && lastResult.Subtype != "success" {
			outcome = fmt.Sprintf("result %s after %s", lastResult.Subtype, time.Since(turnStart).Round(time.Second))
			isErr = true
		}
		logActivity(endCtx, h.store, store.ActivityEntry{
			Persona: "otto", TurnKey: tk, Kind: store.ActivityTurnEnd,
			Detail: outcome, IsError: isErr,
		})
	}

	// Record the latest observed context-token count so the rotator can decide
	// whether the session has grown large enough to clear. ContextTokens sums
	// the cache fields, so it reflects true occupancy under prompt caching.
	// Only update when a result event actually arrived: an errored or aborted
	// turn (subprocess crash, timeout, parse failure) emits none, and writing
	// 0 there would wrongly reset the rotator's view of session size — leaving
	// a large session unrotated because shouldRotate ignores a zero count.
	if gotResult {
		h.otto.setInputTokens(lastResult.ContextTokens)
		recordUsage(sendCtx, h.store, args.Source, args.Model, lastResult)
		// Soft guard: a single agentic turn can't be interrupted mid-flight,
		// so a heavy one (e.g. a long tool-calling task) can balloon the
		// session far past the rotation budget before the rotator gets a free
		// tick to clear it. Surface that in the log so a runaway turn is
		// visible rather than silent; the rotator clears it on the next tick.
		if h.rotate.ctxTokens > 0 && lastResult.ContextTokens > h.rotate.ctxTokens {
			log.Printf("warning: turn used %d context tokens — over the %d rotation budget; session will rotate at the next free tick",
				lastResult.ContextTokens, h.rotate.ctxTokens)
		}
	}

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
	// Log turns only on the success path (reached after the error/non-success
	// early returns above), so session_search surfaces real conversation
	// content rather than "⚠️ Claude error" noise. Keep these here, not earlier.
	logTurn(sendCtx, h.store, h.embedder, "otto", "user", args.Prompt)
	logTurn(sendCtx, h.store, h.embedder, "otto", "assistant", out)
	if len(lastResult.PermissionDenials) > 0 {
		h.surfaceDenials(sendCtx, chatID, lastResult.PermissionDenials)
	}
}
