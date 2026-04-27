//go:build unix

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/permissions"
	"otto/internal/telegram"
)

type fakeBot struct {
	updates  [][]telegram.Update
	idx      int
	sent     []sentMsg
	answered []string // queryIDs answered via AnswerCallbackQuery
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
	u := f.updates[f.idx]
	f.idx++
	return u, nil
}

func (f *fakeBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.sent = append(f.sent, sentMsg{chatID: chatID, text: text})
	return nil
}

func (f *fakeBot) SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]telegram.InlineButton) error {
	f.sent = append(f.sent, sentMsg{chatID: chatID, text: text, buttons: buttons})
	return nil
}

func (f *fakeBot) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
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
	return &handler{
		bot:          bot,
		allow:        auth.New(99),
		session:      sess,
		runner:       runner,
		pending:      permissions.New(8),
		settingsPath: filepath.Join(dir, "settings.json"),
		startedAt:    time.Now(),
	}
}

// runForBriefWindow runs the polling loop for a short, deterministic window,
// then returns. The fakeBot blocks on ctx.Done() once updates are exhausted,
// so the loop returns as soon as ctx expires.
func runForBriefWindow(t *testing.T, h *handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = h.runPollingLoop(ctx)
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

func TestHandlerSurfacesPermissionDenialButtons(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "check email"}}},
	}
	runner := &fakeRunner{
		respond: "I tried but got blocked",
		resultEv: &claude.ResultEvent{
			Subtype: "success",
			PermissionDenials: []claude.PermissionDenial{
				{ToolName: "mcp__gmail-personal__search_emails", ToolUseID: "tu_abc"},
			},
		},
	}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	// Expect ≥2 sends: the assistant text reply, then the buttons prompt.
	if len(bot.sent) < 2 {
		t.Fatalf("expected ≥2 sends, got %d", len(bot.sent))
	}
	// The button message should be the last one and have a non-nil keyboard.
	last := bot.sent[len(bot.sent)-1]
	if last.buttons == nil {
		t.Fatalf("last message had no buttons; sent = %+v", bot.sent)
	}
	if !strings.Contains(last.text, "mcp__gmail-personal__search_emails") {
		t.Errorf("denial text doesn't name the tool: %q", last.text)
	}
	// One row, three buttons: Once / Always / Skip.
	if len(last.buttons) != 1 || len(last.buttons[0]) != 3 {
		t.Fatalf("buttons shape = %v, want 1 row of 3", last.buttons)
	}
	for i, want := range []string{":once", ":always", ":deny"} {
		got := last.buttons[0][i].CallbackData
		if !strings.HasSuffix(got, want) {
			t.Errorf("button[%d] callback = %q, want suffix %q", i, got, want)
		}
		if !strings.HasPrefix(got, "perm:") {
			t.Errorf("button[%d] callback = %q, want perm: prefix", i, got)
		}
	}
}

func TestHandlerCallbackAllowAlwaysWritesSettingsAndReplays(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{respond: "after-replay reply"}
	h := newTestHandler(t, bot, runner)

	id := h.pending.Add(permissions.Entry{
		ToolName:  "mcp__gmail-personal__search_emails",
		Pattern:   "mcp__gmail-personal__*",
		ChatID:    100,
		Prompt:    "check my email",
		SessionID: "sess-x",
	})
	bot.updates = [][]telegram.Update{{{
		UpdateID:        1,
		ChatID:          100,
		UserID:          99,
		CallbackQueryID: "cbq_xyz",
		CallbackData:    "perm:" + id + ":always",
	}}}

	runForBriefWindow(t, h)

	if len(bot.answered) != 1 || bot.answered[0] != "cbq_xyz" {
		t.Errorf("answered = %v, want [cbq_xyz]", bot.answered)
	}
	// Settings file written.
	got := readJSON(t, h.settingsPath)
	allow := got["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 1 || allow[0].(string) != "mcp__gmail-personal__*" {
		t.Errorf("allow = %v", allow)
	}
	// Replay actually invoked claude with the original prompt + the
	// just-approved allowed-tools pattern.
	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1 (replay)", len(runner.called))
	}
	if runner.called[0].Prompt != "check my email" {
		t.Errorf("replay prompt = %q, want stored prompt", runner.called[0].Prompt)
	}
	if len(runner.called[0].AllowedTools) != 1 || runner.called[0].AllowedTools[0] != "mcp__gmail-personal__*" {
		t.Errorf("replay AllowedTools = %v, want [mcp__gmail-personal__*]", runner.called[0].AllowedTools)
	}
	// User saw the replay's reply.
	sawReply := false
	for _, m := range bot.sent {
		if m.text == "after-replay reply" {
			sawReply = true
		}
	}
	if !sawReply {
		t.Errorf("user did not receive replay reply; sent = %+v", bot.sent)
	}
}

func TestHandlerCallbackAllowOnceReplaysWithoutWritingSettings(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{respond: "replay reply"}
	h := newTestHandler(t, bot, runner)

	id := h.pending.Add(permissions.Entry{
		ToolName:  "Bash",
		Pattern:   "Bash",
		ChatID:    100,
		Prompt:    "list files",
		SessionID: "sess-x",
	})
	bot.updates = [][]telegram.Update{{{
		UpdateID:        1,
		ChatID:          100,
		UserID:          99,
		CallbackQueryID: "cbq_xyz",
		CallbackData:    "perm:" + id + ":once",
	}}}

	runForBriefWindow(t, h)

	// settings.json must NOT have been written for once-only.
	if _, err := os.Stat(h.settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings file should not exist after once: %v", err)
	}
	// But replay did happen, with --allowed-tools set.
	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if len(runner.called[0].AllowedTools) != 1 || runner.called[0].AllowedTools[0] != "Bash" {
		t.Errorf("replay AllowedTools = %v", runner.called[0].AllowedTools)
	}
}

func TestHandlerCallbackDenyDoesNotReplay(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)

	id := h.pending.Add(permissions.Entry{
		ToolName:  "Bash",
		Pattern:   "Bash",
		ChatID:    100,
		Prompt:    "list files",
		SessionID: "sess-x",
	})
	bot.updates = [][]telegram.Update{{{
		UpdateID:        1,
		ChatID:          100,
		UserID:          99,
		CallbackQueryID: "cbq_xyz",
		CallbackData:    "perm:" + id + ":deny",
	}}}

	runForBriefWindow(t, h)

	if len(bot.answered) != 1 {
		t.Errorf("answered = %v, want 1 entry", bot.answered)
	}
	if _, err := os.Stat(h.settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings file should not exist after deny: %v", err)
	}
	if len(runner.called) != 0 {
		t.Errorf("runner should not be called on deny: %d invocations", len(runner.called))
	}
}

func TestHandlerCallbackUnknownIDIsAck(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)

	bot.updates = [][]telegram.Update{{{
		UpdateID:        1,
		ChatID:          100,
		UserID:          99,
		CallbackQueryID: "cbq_xyz",
		CallbackData:    "perm:nonexistent:once",
	}}}

	runForBriefWindow(t, h)

	if len(bot.answered) != 1 {
		t.Errorf("answered = %v, want 1", bot.answered)
	}
	if _, err := os.Stat(h.settingsPath); !os.IsNotExist(err) {
		t.Errorf("settings file should not exist for unknown ID: %v", err)
	}
	if len(runner.called) != 0 {
		t.Errorf("runner should not be called for unknown ID")
	}
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
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
