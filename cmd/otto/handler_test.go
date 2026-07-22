//go:build unix

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/memory"
	"otto/internal/store"
	"otto/internal/telegram"
)

type fakeBot struct {
	updates [][]telegram.Update
	idx     int
	sent    []sentMsg
	mu      sync.Mutex
	// betweenBatches is an optional pause inserted before returning the
	// 2nd, 3rd, ... batch. Multi-batch tests use this to ensure each
	// batch's async dispatches complete before the next batch arrives,
	// preserving Otto's serialization semantics for tests that depend on
	// session-ID handoff between calls.
	betweenBatches time.Duration
}

type sentMsg struct {
	chatID int64
	text   string
}

func (f *fakeBot) GetUpdates(ctx context.Context, offset int) ([]telegram.Update, error) {
	if f.idx >= len(f.updates) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.idx > 0 && f.betweenBatches > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(f.betweenBatches):
		}
	}
	u := f.updates[f.idx]
	f.idx++
	return u, nil
}

func (f *fakeBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatID: chatID, text: text})
	return nil
}

func (f *fakeBot) SendMessageHTML(ctx context.Context, chatID int64, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatID: chatID, text: text})
	return nil
}

func (f *fakeBot) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return nil, "", errors.New("not used")
}

type fakeRunner struct {
	respond     string
	failErr     error
	called      []claude.RunArgs
	resultEv    *claude.ResultEvent // optional: emit this result instead of success
	emitSession string              // optional: emit a SessionEvent with this ID before assistant text
}

func (r *fakeRunner) Run(ctx context.Context, args claude.RunArgs) error {
	r.called = append(r.called, args)
	if r.failErr != nil {
		return r.failErr
	}
	if r.emitSession != "" {
		args.Events <- claude.SessionEvent{ID: r.emitSession}
	}
	if r.respond != "" {
		args.Events <- claude.AssistantTextEvent{Text: r.respond}
	}
	if r.resultEv != nil {
		args.Events <- *r.resultEv
	} else {
		args.Events <- claude.ResultEvent{Subtype: "success"}
	}
	return nil
}

func (r *fakeRunner) WithEnv(extra map[string]string) claude.Runner { return r }

func newTestHandler(t *testing.T, bot telegram.BotClient, runner claude.Runner) *handler {
	t.Helper()
	dir := t.TempDir()
	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	totoSess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	return &handler{
		bot:       bot,
		allow:     auth.New(99),
		session:   sess,
		runner:    runner,
		startedAt: time.Now(),
		otto:      newOttoState(),
		toto: &Toto{
			bot:     bot,
			runner:  runner,
			session: totoSess,
			persona: "test toto",
		},
	}
}

// runForBriefWindow runs the polling loop for a short, deterministic window,
// then waits for any in-flight dispatch goroutines to finish before returning.
// The fakeBot blocks on ctx.Done() once updates are exhausted, so the loop
// returns as soon as ctx expires.
func runForBriefWindow(t *testing.T, h *handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = h.runPollingLoop(ctx)
	h.WaitDispatches()
}

func TestHandlerForwardsTextMessage(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{respond: "hello!"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if runner.called[0].Prompt != "hi" {
		t.Errorf("prompt = %q", runner.called[0].Prompt)
	}
	if runner.called[0].SessionID != "" {
		t.Errorf("first call SessionID = %q, want empty", runner.called[0].SessionID)
	}
	if len(bot.sent) != 1 || bot.sent[0].text != "hello!" {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestHandlerCapturesAndReusesSessionID(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{
			{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "first"}},
			{{UpdateID: 2, ChatID: 100, UserID: 99, Text: "second"}},
		},
		// Async dispatch means batch 2 must wait for batch 1's Otto call
		// to release the slot, otherwise message 2 routes to Toto.
		betweenBatches: 50 * time.Millisecond,
	}
	runner := &fakeRunner{respond: "ok", emitSession: "sess-real-uuid"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 2 {
		t.Fatalf("runner called %d times, want 2", len(runner.called))
	}
	if runner.called[0].SessionID != "" {
		t.Errorf("first call SessionID = %q, want empty (no resume on first call)", runner.called[0].SessionID)
	}
	if runner.called[1].SessionID != "sess-real-uuid" {
		t.Errorf("second call SessionID = %q, want sess-real-uuid (captured from first)", runner.called[1].SessionID)
	}
}

func TestHandlerNewCommandClearsSessionAndNextMessageIsFresh(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{
			{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "first"}},
			{{UpdateID: 2, ChatID: 100, UserID: 99, Text: "/new"}},
			{{UpdateID: 3, ChatID: 100, UserID: 99, Text: "after-new"}},
		},
		betweenBatches: 50 * time.Millisecond,
	}
	runner := &fakeRunner{respond: "ok", emitSession: "sess-1"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	// Two real calls (the /new command doesn't invoke the runner).
	if len(runner.called) != 2 {
		t.Fatalf("runner called %d times, want 2", len(runner.called))
	}
	if runner.called[0].SessionID != "" {
		t.Errorf("first call SessionID = %q, want empty", runner.called[0].SessionID)
	}
	if runner.called[1].SessionID != "" {
		t.Errorf("after /new, next call SessionID = %q, want empty (fresh)", runner.called[1].SessionID)
	}
}

func TestHandlerDropsNonAllowlistedUser(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 7, Text: "hi"}}},
	}
	runner := &fakeRunner{respond: "should not run"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 0 {
		t.Errorf("runner was called for non-allowlisted user")
	}
	if len(bot.sent) != 0 {
		t.Errorf("bot sent message to non-allowlisted user")
	}
}

func TestHandlerSendsErrorOnRunnerFailure(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{failErr: errors.New("boom")}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(bot.sent) != 1 {
		t.Fatalf("sent = %+v", bot.sent)
	}
	if !strings.Contains(bot.sent[0].text, "boom") {
		t.Errorf("error message = %q, missing 'boom'", bot.sent[0].text)
	}
}

func TestHandlerSurfacesNonSuccessResult(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{
		resultEv: &claude.ResultEvent{Subtype: "rate_limited", Error: "too many requests"},
	}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(bot.sent) != 1 {
		t.Fatalf("sent = %+v", bot.sent)
	}
	if !strings.Contains(bot.sent[0].text, "rate_limited") {
		t.Errorf("missing subtype in %q", bot.sent[0].text)
	}
	if !strings.Contains(bot.sent[0].text, "too many requests") {
		t.Errorf("missing error in %q", bot.sent[0].text)
	}
}

type fakeBotWithDownload struct {
	fakeBot
	files map[string][]byte
	cts   map[string]string
}

func (f *fakeBotWithDownload) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return f.files[fileID], f.cts[fileID], nil
}

func TestSurfaceDenialsAsPlainText(t *testing.T) {
	bot := &fakeBot{}
	// surfaceDenials only touches h.bot — no need to wire other fields.
	h := &handler{bot: bot}

	denials := []claude.PermissionDenial{
		{ToolName: "mcp__gmail-personal__send_message", ToolUseID: "tu_1"},
		{ToolName: "mcp__gmail-personal__search_emails", ToolUseID: "tu_2"},
		{ToolName: "Bash", ToolUseID: "tu_3"},
	}
	h.surfaceDenials(context.Background(), 999, denials)

	// Both gmail tools share the wildcard pattern, so we expect one message
	// for the family + one for Bash = 2 messages total.
	if len(bot.sent) != 2 {
		t.Fatalf("got %d messages, want 2", len(bot.sent))
	}
	want0 := "mcp__gmail-personal__*"
	want1 := "Bash"
	if !strings.Contains(bot.sent[0].text, want0) {
		t.Errorf("msg 0 missing pattern %q: %q", want0, bot.sent[0].text)
	}
	if !strings.Contains(bot.sent[1].text, want1) {
		t.Errorf("msg 1 missing pattern %q: %q", want1, bot.sent[1].text)
	}
	for _, m := range bot.sent {
		if !strings.Contains(m.text, "permissions.allow") {
			t.Errorf("msg missing settings.json hint: %q", m.text)
		}
	}
}

func TestHandlerInjectsMemoryAndLogsTurns(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "what's my flight"}}},
	}
	runner := &fakeRunner{respond: "your flight is at 9am"}
	h := newTestHandler(t, bot, runner)

	dir := t.TempDir()
	core := memory.NewCore(dir, 2200, 1375)
	if err := core.Add(memory.TargetUser, "User flies to Tokyo on Friday."); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.mem = core
	h.store = st
	h.baseSystemPrompt = "BASE PERSONA"

	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	asp := runner.called[0].AppendSystemPrompt
	if !strings.Contains(asp, "BASE PERSONA") || !strings.Contains(asp, "Tokyo on Friday") {
		t.Errorf("AppendSystemPrompt missing base or memory: %q", asp)
	}
	if !strings.Contains(asp, "CURRENT TIME") {
		t.Errorf("AppendSystemPrompt missing time block: %q", asp)
	}
	ctx := context.Background()
	if got, _ := st.SearchFTS(ctx, "flight", 5); len(got) == 0 {
		t.Error("user turn not logged")
	}
	if got, _ := st.SearchFTS(ctx, "9am", 5); len(got) == 0 {
		t.Error("assistant turn not logged")
	}
}

func TestHandlerForwardsPhotoToClaude(t *testing.T) {
	bot := &fakeBotWithDownload{
		fakeBot: fakeBot{
			updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "describe this", PhotoIDs: []string{"PHOTO-1"}}}},
		},
		files: map[string][]byte{"PHOTO-1": []byte("\x89PNG fake")},
		cts:   map[string]string{"PHOTO-1": "image/png"},
	}
	runner := &fakeRunner{respond: "an image"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner.called = %d", len(runner.called))
	}
	if len(runner.called[0].ImagePaths) != 1 {
		t.Fatalf("image paths = %v", runner.called[0].ImagePaths)
	}
	if !strings.HasSuffix(runner.called[0].ImagePaths[0], ".png") {
		t.Errorf("image path = %q (want .png)", runner.called[0].ImagePaths[0])
	}
	// File should be cleaned up after the call.
	if _, err := os.Stat(runner.called[0].ImagePaths[0]); !os.IsNotExist(err) {
		t.Errorf("temp file not cleaned up: %v", err)
	}
}

func TestOttoStateTokenAndActivityTracking(t *testing.T) {
	s := newOttoState()
	s.setInputTokens(1234)
	s.markUserMessage()
	tokens, idle := s.rotationSnapshot()
	if tokens != 1234 {
		t.Errorf("tokens = %d, want 1234", tokens)
	}
	if idle > time.Second {
		t.Errorf("idle = %s, want ~0 right after markUserMessage", idle)
	}
	s.resetInputTokens()
	tokens, _ = s.rotationSnapshot()
	if tokens != 0 {
		t.Errorf("after reset tokens = %d, want 0", tokens)
	}
}

func TestErroredTurnDoesNotResetTokenCount(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{failErr: errors.New("boom")} // errors before any ResultEvent
	h := newTestHandler(t, bot, runner)
	h.otto.setInputTokens(180000) // simulate a large in-flight session
	runForBriefWindow(t, h)
	if got := h.otto.lastInputTokens; got != 180000 {
		t.Errorf("token count after errored turn = %d, want 180000 (must not reset to 0)", got)
	}
}

func TestSuccessfulTurnUpdatesTokenCount(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{respond: "ok", resultEv: &claude.ResultEvent{Subtype: "success", ContextTokens: 50000}}
	h := newTestHandler(t, bot, runner)
	h.otto.setInputTokens(180000)
	runForBriefWindow(t, h)
	if got := h.otto.lastInputTokens; got != 50000 {
		t.Errorf("token count after successful turn = %d, want 50000", got)
	}
}

// TestCancelInflightGuardsAgainstStaleGeneration verifies the TOCTOU fix:
// a cancel request carrying the generation of an already-finished turn must
// not cancel (or suppress the error of) a newer turn that has since claimed
// the slot.
func TestCancelInflightGuardsAgainstStaleGeneration(t *testing.T) {
	s := newOttoState()

	// Turn A acquires the slot; capture its generation.
	if !s.tryAcquire("A") {
		t.Fatal("tryAcquire A should succeed on a fresh state")
	}
	genA := s.gen

	// Turn A finishes and a fresh turn B claims the slot with a live cancel.
	s.release()
	if !s.tryAcquire("B") {
		t.Fatal("tryAcquire B should succeed after release")
	}
	cancelled := false
	s.setCancel(func() { cancelled = true })

	// A stale cancel carrying genA must no-op: B is a different generation.
	if s.cancelInflight(genA) {
		t.Error("cancelInflight(genA) should return false for a stale generation")
	}
	if cancelled {
		t.Error("stale cancelInflight must not cancel the newer turn B")
	}
	if s.shouldSuppressError() {
		t.Error("stale cancelInflight must not set suppressError for turn B")
	}

	// The correct generation cancels B and suppresses its error.
	if !s.cancelInflight(s.gen) {
		t.Fatal("cancelInflight with B's own generation should return true")
	}
	if !cancelled {
		t.Error("cancelInflight should have cancelled turn B")
	}
	if !s.shouldSuppressError() {
		t.Error("cancelInflight should have set suppressError for turn B")
	}
}

// TestCancelInflightNoopWhenCancelNotYetRegistered verifies that during the
// acquire→setCancel window (busy=true, gen set, but cancel==nil) cancelInflight
// reports failure and leaves suppressError untouched, rather than falsely
// claiming success and poisoning the turn.
func TestCancelInflightNoopWhenCancelNotYetRegistered(t *testing.T) {
	s := newOttoState()
	if !s.tryAcquire("A") {
		t.Fatal("tryAcquire A should succeed on a fresh state")
	}
	// setCancel has not run yet: busy but no cancel func registered.
	if s.cancelInflight(s.gen) {
		t.Error("cancelInflight should return false before the cancel func is registered")
	}
	if s.shouldSuppressError() {
		t.Error("cancelInflight must not set suppressError during the registration window")
	}
	// Once the cancel func is registered, a retry succeeds.
	cancelled := false
	s.setCancel(func() { cancelled = true })
	if !s.cancelInflight(s.gen) {
		t.Fatal("cancelInflight should succeed after the cancel func is registered")
	}
	if !cancelled {
		t.Error("cancelInflight should have invoked the registered cancel func")
	}
}

// TestCancelInflightNoopWhenIdle verifies cancelInflight is a no-op (returns
// false) when Otto isn't busy, so /restart and the watchdog can skip their
// user-facing "interrupted" messages.
func TestCancelInflightNoopWhenIdle(t *testing.T) {
	s := newOttoState()
	if s.cancelInflight(0) {
		t.Error("cancelInflight on an idle state should return false")
	}
}

func TestRunRotatorClearsLargeIdleSession(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)
	h.rotate = rotateConfig{ctxTokens: 1000, hard: 0.85, idleWindow: 0}
	if err := h.session.Set("sess-xyz"); err != nil {
		t.Fatal(err)
	}
	h.otto.setInputTokens(900) // 90% → over hard

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runRotator(ctx)

	deadline := time.After(3 * time.Second)
	for {
		if h.session.ID() == "" {
			break // rotated
		}
		select {
		case <-deadline:
			t.Fatal("session was not rotated within 3s")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestPartitionPetLast covers the batch reorder in isolation: pet-addressed
// updates move to the back, relative order is preserved inside each group,
// and photos are never treated as pet-addressed (they always go to Otto).
func TestPartitionPetLast(t *testing.T) {
	pets := newPetRegistry(&Toto{}, &Toot{})

	mk := func(id int, text string) telegram.Update {
		return telegram.Update{UpdateID: id, UserID: 99, ChatID: 99, Text: text}
	}

	t.Run("pet-first-batch-is-reordered", func(t *testing.T) {
		in := []telegram.Update{
			mk(1, "hey toto what's otto doing?"),
			mk(2, "summarize all my emails"),
		}
		got := partitionPetLast(in, pets)
		if len(got) != 2 || got[0].UpdateID != 2 || got[1].UpdateID != 1 {
			t.Fatalf("want [2 1], got %v", ids(got))
		}
	})

	t.Run("preserves-order-within-groups", func(t *testing.T) {
		in := []telegram.Update{
			mk(1, "toto hi"),
			mk(2, "first otto message"),
			mk(3, "toot status?"),
			mk(4, "second otto message"),
		}
		got := partitionPetLast(in, pets)
		if want := []int{2, 4, 1, 3}; !equalIDs(got, want) {
			t.Fatalf("want %v, got %v", want, ids(got))
		}
	})

	t.Run("photos-are-never-pet-addressed", func(t *testing.T) {
		// Caption names a pet, but photos always route to Otto.
		u := telegram.Update{UpdateID: 1, Text: "toto look at this", PhotoIDs: []string{"f1"}}
		if isPetAddressed(u, pets) {
			t.Error("photo update classified as pet-addressed")
		}
	})

	t.Run("nil-registry-is-a-noop", func(t *testing.T) {
		in := []telegram.Update{mk(1, "toto hi"), mk(2, "otto hi")}
		got := partitionPetLast(in, nil)
		if !equalIDs(got, []int{1, 2}) {
			t.Fatalf("nil registry should preserve order, got %v", ids(got))
		}
	})

	t.Run("commands-are-non-pet", func(t *testing.T) {
		in := []telegram.Update{mk(1, "toto hi"), mk(2, "/status")}
		got := partitionPetLast(in, pets)
		if !equalIDs(got, []int{2, 1}) {
			t.Fatalf("want commands dispatched before pets, got %v", ids(got))
		}
	})
}

func ids(us []telegram.Update) []int {
	out := make([]int, len(us))
	for i, u := range us {
		out[i] = u.UpdateID
	}
	return out
}

func equalIDs(us []telegram.Update, want []int) bool {
	if len(us) != len(want) {
		return false
	}
	for i, u := range us {
		if u.UpdateID != want[i] {
			return false
		}
	}
	return true
}

// blockingRunner holds each Run call open until release is closed, so a test
// can observe Otto mid-turn. Signals started on the first call.
type blockingRunner struct {
	mu      sync.Mutex
	called  []claude.RunArgs
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockingRunner) Run(ctx context.Context, args claude.RunArgs) error {
	r.mu.Lock()
	r.called = append(r.called, args)
	r.mu.Unlock()
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	args.Events <- claude.AssistantTextEvent{Text: "done"}
	args.Events <- claude.ResultEvent{Subtype: "success"}
	return nil
}

func (r *blockingRunner) WithEnv(map[string]string) claude.Runner { return r }

// recordingRunner is a fakeRunner with a mutex-guarded call log and a signal
// fired on the first call, for use from a dispatch goroutine.
type recordingRunner struct {
	mu     sync.Mutex
	called []claude.RunArgs
	fired  chan struct{}
	once   sync.Once
}

func (r *recordingRunner) Run(ctx context.Context, args claude.RunArgs) error {
	r.mu.Lock()
	r.called = append(r.called, args)
	r.mu.Unlock()
	args.Events <- claude.AssistantTextEvent{Text: "mrow"}
	args.Events <- claude.ResultEvent{Subtype: "success"}
	r.once.Do(func() { close(r.fired) })
	return nil
}

func (r *recordingRunner) WithEnv(map[string]string) claude.Runner { return r }

func (r *recordingRunner) first() claude.RunArgs {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.called) == 0 {
		return claude.RunArgs{}
	}
	return r.called[0]
}

// TestPetLastBatchSeesOttoBusy is the end-to-end version of the reorder fix:
// a batch delivered as [pet-addressed, Otto-bound] must still leave Toto
// reporting Otto as BUSY. Before the partition, Toto dispatched first,
// snapshotted an idle Otto, and told the user nothing was going on.
func TestPetLastBatchSeesOttoBusy(t *testing.T) {
	ottoRunner := &blockingRunner{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	totoRunner := &recordingRunner{fired: make(chan struct{})}

	dir := t.TempDir()
	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	totoSess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}

	bot := &fakeBot{updates: [][]telegram.Update{{
		// Telegram delivered the pet message FIRST — the inverted order
		// that used to defeat the 150ms spacing fix.
		{UpdateID: 1, UserID: 99, ChatID: 99, Text: "hey toto what's otto doing?"},
		{UpdateID: 2, UserID: 99, ChatID: 99, Text: "summarize all my emails"},
	}}}

	toto := &Toto{bot: bot, runner: totoRunner, session: totoSess, persona: "cat"}
	h := &handler{
		bot:       bot,
		allow:     auth.New(99),
		session:   sess,
		runner:    ottoRunner,
		startedAt: time.Now(),
		otto:      newOttoState(),
		toto:      toto,
		pets:      newPetRegistry(toto),
	}
	toto.ottoStatus = h.otto.Snapshot

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.runPollingLoop(ctx) }()

	// Otto must be the one to claim the slot first, despite arriving second.
	select {
	case <-ottoRunner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Otto never started — the Otto-bound message was not dispatched first")
	}
	select {
	case <-totoRunner.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("Toto never replied")
	}

	got := totoRunner.first().Prompt
	if !strings.Contains(got, "otto status: busy") {
		t.Errorf("Toto did not see Otto as busy; prompt was:\n%s", got)
	}
	if !strings.Contains(got, "summarize all my emails") {
		t.Errorf("Toto's status note missing Otto's in-flight prompt:\n%s", got)
	}

	close(ottoRunner.release)
	cancel()
	h.WaitDispatches()
}
