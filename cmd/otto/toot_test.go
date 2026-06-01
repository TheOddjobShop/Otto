//go:build unix

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/claude"
)

// newTestToot constructs a Toot wired to a fakeBot and a fakeRunner.
// `response` is what the fakeRunner emits as Toot's announcement body.
// Returns the Toot plus the fakeRunner so tests can inspect the args
// passed to Claude.
func newTestToot(t *testing.T, bot *fakeBot, response string) (*Toot, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	sess, err := claude.LoadSession(filepath.Join(dir, "toot-sid"))
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{respond: response}
	return &Toot{
		bot:     bot,
		runner:  runner,
		session: sess,
		persona: "TEST PERSONA",
	}, runner
}

func TestTootAnnounce(t *testing.T) {
	bot := &fakeBot{}
	toot, runner := newTestToot(t, bot, "Release announcement narrated by Toot here.")

	if err := toot.Announce(context.Background(), 42, "v1.0.0", "v1.0.1", "What's Changed\n* Item one"); err != nil {
		t.Fatal(err)
	}

	if len(runner.called) != 1 {
		t.Fatalf("runner.called=%d, want 1", len(runner.called))
	}
	args := runner.called[0]
	if args.Model != tootModel {
		t.Errorf("Model=%q, want %q", args.Model, tootModel)
	}
	if args.Effort != tootEffort {
		t.Errorf("Effort=%q, want %q", args.Effort, tootEffort)
	}
	if len(args.DisallowedTools) != 1 || args.DisallowedTools[0] != "*" {
		t.Errorf("DisallowedTools=%v, want [\"*\"]", args.DisallowedTools)
	}
	for _, want := range []string{"v1.0.0", "v1.0.1", "Item one", "TEST PERSONA", "CURRENT TIME"} {
		if !strings.Contains(args.AppendSystemPrompt, want) {
			t.Errorf("system prompt missing %q: %q", want, args.AppendSystemPrompt)
		}
	}

	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	for _, want := range []string{"<blockquote>", "TOOT", "Release announcement narrated by Toot here."} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %q", want, msg)
		}
	}
}

func TestTootConfirm(t *testing.T) {
	bot := &fakeBot{}
	toot, runner := newTestToot(t, bot, "")

	if err := toot.Confirm(context.Background(), 42, "v1.0.1"); err != nil {
		t.Fatal(err)
	}

	if len(runner.called) != 0 {
		t.Errorf("runner.called=%d, want 0 (Confirm is templated, no Claude call)", len(runner.called))
	}
	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	// New phrasing keeps the tag, keeps a "Restart"-ish word, and promises
	// the boot-back-online ping so the user expects ~10s of downtime
	// instead of assuming Otto is hung.
	for _, want := range []string{"v1.0.1", "TOOT", "Restarting", "ping back"} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirm missing %q: %q", want, msg)
		}
	}
}

func TestTootAnnounceFallbackOnRunnerError(t *testing.T) {
	bot := &fakeBot{}
	dir := t.TempDir()
	sess, _ := claude.LoadSession(filepath.Join(dir, "toot-sid"))
	toot := &Toot{
		bot:     bot,
		runner:  &fakeRunner{failErr: errors.New("claude oops")},
		session: sess,
	}

	if err := toot.Announce(context.Background(), 42, "v1.0.0", "v1.0.1", ""); err != nil {
		t.Fatalf("Announce should fall back on runner error, got: %v", err)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1 (static fallback)", len(bot.sent))
	}
	if !strings.Contains(bot.sent[0].text, "v1.0.1") {
		t.Errorf("fallback missing tag: %q", bot.sent[0].text)
	}
}

func TestTootDeliverEscapesHTMLInBody(t *testing.T) {
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "")

	if err := toot.deliver(context.Background(), 42, "<script>alert(1)</script>"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bot.sent[0].text, "&lt;script&gt;") {
		t.Errorf("expected HTML-escaped body: %q", bot.sent[0].text)
	}
}

func TestStripUpdateMarker(t *testing.T) {
	cases := []struct {
		in      string
		wantOut string
		wantHit bool
	}{
		{"Hello world", "Hello world", false},
		{"Initiating install. [TRIGGER_UPDATE]", "Initiating install.", true},
		{"[TRIGGER_UPDATE]", "", true},
		{"Right.\n\n[TRIGGER_UPDATE]", "Right.", true},
		{"two markers [TRIGGER_UPDATE] both [TRIGGER_UPDATE]", "two markers  both", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			out, hit := stripUpdateMarker(c.in)
			if hit != c.wantHit {
				t.Errorf("hit=%v, want %v", hit, c.wantHit)
			}
			if out != c.wantOut {
				t.Errorf("out=%q, want %q", out, c.wantOut)
			}
		})
	}
}

func TestTootReplyTriggersUpdateOnMarker(t *testing.T) {
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "Initiating install of v0.2.0, sir. Stand by. [TRIGGER_UPDATE]")

	triggered := make(chan struct{}, 1)
	toot.pendingUpdate = func() *pendingUpdate {
		return &pendingUpdate{Tag: "v0.2.0", AssetURL: "https://x/asset"}
	}
	toot.triggerUpdate = func() { triggered <- struct{}{} }

	toot.Reply(context.Background(), 42, "toot, do it")

	// Visible message has marker stripped.
	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	if strings.Contains(bot.sent[0].text, "TRIGGER_UPDATE") {
		t.Errorf("marker leaked to user: %q", bot.sent[0].text)
	}
	if !strings.Contains(bot.sent[0].text, "Initiating install") {
		t.Errorf("missing install-cue body: %q", bot.sent[0].text)
	}

	// triggerUpdate was called (the goroutine fires after deliver).
	select {
	case <-triggered:
	case <-time.After(time.Second):
		t.Fatal("triggerUpdate was not called")
	}
}

func TestTootReplyDoesNotTriggerWithoutMarker(t *testing.T) {
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "Noted, sir. The release covers minor housekeeping.")

	triggered := make(chan struct{}, 1)
	toot.pendingUpdate = func() *pendingUpdate {
		return &pendingUpdate{Tag: "v0.2.0", AssetURL: "https://x/asset"}
	}
	toot.triggerUpdate = func() { triggered <- struct{}{} }

	toot.Reply(context.Background(), 42, "toot, what changed?")

	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	select {
	case <-triggered:
		t.Fatal("triggerUpdate fired when marker was absent")
	case <-time.After(50 * time.Millisecond):
		// expected: no trigger
	}
}

func TestTootReplyPromptsPendingUpdateWhenAvailable(t *testing.T) {
	bot := &fakeBot{}
	toot, runner := newTestToot(t, bot, "noted")
	toot.pendingUpdate = func() *pendingUpdate {
		return &pendingUpdate{Tag: "v0.2.0"}
	}

	toot.Reply(context.Background(), 42, "hey")

	if len(runner.called) != 1 {
		t.Fatalf("runner.called=%d, want 1", len(runner.called))
	}
	prompt := runner.called[0].AppendSystemPrompt
	// Tool framing + tag + marker + the loosened-judgment guidance.
	for _, want := range []string{
		"install_update",
		"v0.2.0",
		tootUpdateMarker,
		"any reasonable form", // loosened judgment language
		"can you update",      // example of polite phrasing accepted
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestTootReplySurfacesCurrentVersion(t *testing.T) {
	bot := &fakeBot{}
	toot, runner := newTestToot(t, bot, "noted")
	toot.version = "v9.9.9-test"

	toot.Reply(context.Background(), 42, "toot what version are we on")

	if len(runner.called) != 1 {
		t.Fatalf("runner.called=%d, want 1", len(runner.called))
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "v9.9.9-test") {
		t.Errorf("prompt missing current version: %q", runner.called[0].AppendSystemPrompt)
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "CURRENT TIME") {
		t.Errorf("toot reply prompt missing time block: %q", runner.called[0].AppendSystemPrompt)
	}
}

func TestStripCheckMarker(t *testing.T) {
	cases := []struct {
		in      string
		wantOut string
		wantHit bool
	}{
		{"Hello world", "Hello world", false},
		{"Checking, sir. [CHECK_FOR_UPDATE]", "Checking, sir.", true},
		{"[CHECK_FOR_UPDATE]", "", true},
		{"Right.\n\n[CHECK_FOR_UPDATE]", "Right.", true},
		{"two markers [CHECK_FOR_UPDATE] both [CHECK_FOR_UPDATE]", "two markers  both", true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			out, hit := stripCheckMarker(c.in)
			if hit != c.wantHit {
				t.Errorf("hit=%v, want %v", hit, c.wantHit)
			}
			if out != c.wantOut {
				t.Errorf("out=%q, want %q", out, c.wantOut)
			}
		})
	}
}

func TestTootReplyChecksWhenMarkerPresent(t *testing.T) {
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "Checking, sir. [CHECK_FOR_UPDATE]")

	triggered := make(chan struct{}, 1)
	toot.checkNow = func(ctx context.Context) *pendingUpdate {
		return &pendingUpdate{Tag: "v0.3.4"}
	}
	toot.triggerUpdate = func() { triggered <- struct{}{} }

	toot.Reply(context.Background(), 42, "toot, check for updates")

	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if strings.Contains(msg, "CHECK_FOR_UPDATE") {
		t.Errorf("marker leaked to user: %q", msg)
	}
	if !strings.Contains(msg, "Update found: v0.3.4.") {
		t.Errorf("missing check result line: %q", msg)
	}

	select {
	case <-triggered:
		t.Fatal("triggerUpdate fired when only [CHECK_FOR_UPDATE] was emitted")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestTootReplyChecksAndInstallsWhenBothMarkersPresent(t *testing.T) {
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "On it, sir. [CHECK_FOR_UPDATE]\n[TRIGGER_UPDATE]")

	triggered := make(chan struct{}, 1)
	toot.checkNow = func(ctx context.Context) *pendingUpdate {
		return &pendingUpdate{Tag: "v0.3.5"}
	}
	toot.triggerUpdate = func() { triggered <- struct{}{} }

	toot.Reply(context.Background(), 42, "toot, check and install if there's one")

	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if strings.Contains(msg, "CHECK_FOR_UPDATE") || strings.Contains(msg, "TRIGGER_UPDATE") {
		t.Errorf("marker leaked to user: %q", msg)
	}
	if !strings.Contains(msg, "Update found: v0.3.5.") {
		t.Errorf("missing check result line: %q", msg)
	}

	select {
	case <-triggered:
		// expected
	case <-time.After(time.Second):
		t.Fatal("triggerUpdate was not called when both markers present and check found update")
	}
}

func TestTootReplyCheckPlusInstallSkipsInstallWhenNothingFound(t *testing.T) {
	bot := &fakeBot{}
	toot, _ := newTestToot(t, bot, "On it. [CHECK_FOR_UPDATE]\n[TRIGGER_UPDATE]")

	triggered := make(chan struct{}, 1)
	toot.checkNow = func(ctx context.Context) *pendingUpdate { return nil }
	toot.triggerUpdate = func() { triggered <- struct{}{} }

	toot.Reply(context.Background(), 42, "toot, check and install if there's one")

	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if !strings.Contains(msg, "Up to date as of ") {
		t.Errorf("missing 'Up to date' line: %q", msg)
	}

	select {
	case <-triggered:
		t.Fatal("triggerUpdate fired even though CheckNow returned nil")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestTootReplyOmitsCheckMarkerInstructionsWhenCheckNowNil(t *testing.T) {
	bot := &fakeBot{}
	toot, runner := newTestToot(t, bot, "noted")
	// checkNow stays nil — the prompt must not mention [CHECK_FOR_UPDATE].

	toot.Reply(context.Background(), 42, "hey")

	if len(runner.called) != 1 {
		t.Fatalf("runner.called=%d, want 1", len(runner.called))
	}
	prompt := runner.called[0].AppendSystemPrompt
	if strings.Contains(prompt, tootCheckMarker) {
		t.Errorf("prompt should NOT mention check marker when checkNow is nil: %q", prompt)
	}
	if strings.Contains(prompt, "check_for_update") {
		t.Errorf("prompt should NOT mention check_for_update tool when checkNow is nil")
	}
}

func TestTootReplyPromptsNoPendingWhenAbsent(t *testing.T) {
	bot := &fakeBot{}
	toot, runner := newTestToot(t, bot, "noted")
	toot.pendingUpdate = func() *pendingUpdate { return nil }

	toot.Reply(context.Background(), 42, "hey")

	prompt := runner.called[0].AppendSystemPrompt
	if !strings.Contains(prompt, "no pending update") {
		t.Errorf("prompt should explicitly mention no pending update: %q", prompt)
	}
	if strings.Contains(prompt, tootUpdateMarker) {
		t.Errorf("marker instructions should not appear when no update is pending")
	}
}
