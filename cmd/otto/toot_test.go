//go:build unix

package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

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
	for _, want := range []string{"v1.0.0", "v1.0.1", "Item one", "TEST PERSONA"} {
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
	toot, runner := newTestToot(t, bot, "Installation complete in Toot's voice.")

	if err := toot.Confirm(context.Background(), 42, "v1.0.1"); err != nil {
		t.Fatal(err)
	}

	// Confirm now goes through Claude — every line in Toot's banner
	// is real LLM output, no hardcoded confirmation text.
	if len(runner.called) != 1 {
		t.Fatalf("runner.called=%d, want 1 (Confirm now invokes Claude)", len(runner.called))
	}
	args := runner.called[0]
	if !strings.Contains(args.AppendSystemPrompt, "v1.0.1") {
		t.Errorf("system prompt missing version: %q", args.AppendSystemPrompt)
	}
	if args.Model != tootModel {
		t.Errorf("Model=%q, want %q", args.Model, tootModel)
	}

	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	for _, want := range []string{"TOOT", "Installation complete in Toot&#39;s voice."} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirm missing %q: %q", want, msg)
		}
	}
}

func TestTootConfirmFallbackOnRunnerError(t *testing.T) {
	bot := &fakeBot{}
	dir := t.TempDir()
	sess, _ := claude.LoadSession(filepath.Join(dir, "toot-sid"))
	toot := &Toot{
		bot:     bot,
		runner:  &fakeRunner{failErr: errors.New("claude oops")},
		session: sess,
	}

	if err := toot.Confirm(context.Background(), 42, "v1.0.1"); err != nil {
		t.Fatalf("Confirm should fall back, not error: %v", err)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("bot.sent=%d, want 1 (system fallback)", len(bot.sent))
	}
	msg := bot.sent[0].text
	if strings.Contains(msg, "TOOT") {
		t.Errorf("system fallback should NOT use TOOT banner: %q", msg)
	}
	if !strings.Contains(msg, "v1.0.1") {
		t.Errorf("system fallback missing version: %q", msg)
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
		t.Fatalf("bot.sent=%d, want 1 (system fallback)", len(bot.sent))
	}
	msg := bot.sent[0].text
	// Fallback is a system message — must NOT use TOOT banner (no
	// fake voice attribution).
	if strings.Contains(msg, "TOOT") {
		t.Errorf("system fallback should NOT use TOOT banner: %q", msg)
	}
	if !strings.Contains(msg, "v1.0.1") {
		t.Errorf("fallback missing tag: %q", msg)
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
