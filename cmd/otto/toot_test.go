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
	for _, want := range []string{"v1.0.1", "TOOT", "Restarting"} {
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
