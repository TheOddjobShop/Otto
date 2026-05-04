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
	"otto/internal/telegram"
)

type fakeBot struct {
	updates  [][]telegram.Update
	idx      int
	sent     []sentMsg
	answered []string // queryIDs answered via AnswerCallbackQuery
	mu       sync.Mutex
	// betweenBatches is an optional pause inserted before returning the
	// 2nd, 3rd, ... batch. Multi-batch tests use this to ensure each
	// batch's async dispatches complete before the next batch arrives,
	// preserving Otto's serialization semantics for tests that depend on
	// session-ID handoff between calls.
	betweenBatches time.Duration
}

type sentMsg struct {
	chatID  int64
	text    string
	buttons [][]telegram.InlineButton // populated only for SendMessageWithButtons
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

func (f *fakeBot) SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]telegram.InlineButton) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, sentMsg{chatID: chatID, text: text, buttons: buttons})
	return nil
}

func (f *fakeBot) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answered = append(f.answered, queryID)
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
	h.surfaceDenials(context.Background(), 999, "send a test email", denials)

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
