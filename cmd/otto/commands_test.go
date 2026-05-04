//go:build unix

// This file is in the same package as handler_test.go and reuses fakeBot,
// fakeRunner, newTestHandler, and runForBriefWindow from there.

package main

import (
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"otto/internal/telegram"
)

func TestCommandNewClearsSession(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "/new"}}},
	}
	runner := &fakeRunner{respond: "ignored"}
	h := newTestHandler(t, bot, runner)

	// Pre-seed a session to verify /new clears it.
	if err := h.session.Set("preexisting-session"); err != nil {
		t.Fatal(err)
	}
	runForBriefWindow(t, h)

	if h.session.ID() != "" {
		t.Errorf("session not cleared after /new: %q", h.session.ID())
	}
	if len(runner.called) != 0 {
		t.Errorf("runner called %d times for /new, want 0", len(runner.called))
	}
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "new session") {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestCommandWhoami(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "/whoami"}}},
	}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 0 {
		t.Errorf("runner called for /whoami")
	}
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "session=") {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestCommandStatus(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "/status"}}},
	}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 0 {
		t.Errorf("runner called for /status")
	}
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "uptime") {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestVersionCommand(t *testing.T) {
	h := &handler{}
	got := h.tryCommand(context.Background(), telegram.Update{Text: "/version"})
	if !got.handled {
		t.Fatal("expected /version to be handled")
	}
	if !strings.Contains(got.reply, "version=") {
		t.Errorf("reply missing version=: %q", got.reply)
	}
	if !strings.Contains(got.reply, runtime.GOOS) {
		t.Errorf("reply missing GOOS=%s: %q", runtime.GOOS, got.reply)
	}
}

func TestUpdateCommandNoPending(t *testing.T) {
	// No pending update → synchronous reply only, no async goroutine fires.
	h := &handler{
		updater: &updater{currentVersion: "v1.2.3"},
	}
	got := h.tryCommand(context.Background(), telegram.Update{Text: "/update"})
	if !got.handled {
		t.Fatal("not handled")
	}
	if !strings.Contains(got.reply, "No update available") {
		t.Errorf("reply=%q", got.reply)
	}
	if !strings.Contains(got.reply, "v1.2.3") {
		t.Errorf("reply missing current version: %q", got.reply)
	}
}

// newPendingHandler is a shared helper for tests that exercise the
// /update path with a real pending update. It wires both h.bot and
// u.toot (via newToot(bot)) to the same fakeBot so the async goroutine
// spawned by /update has a place to send its failure message (the
// AssetURL is bogus, so the install will fail-fast on DNS, post one
// error message via h.bot, and exit without panicking).
func newPendingHandler(t *testing.T) (*handler, *fakeBot) {
	t.Helper()
	bot := &fakeBot{}
	u := &updater{
		currentVersion: "v1.0.0",
		toot:           newToot(bot),
		chatID:         42,
		exitFunc:       func() {}, // never actually exit during tests
		exePath:        func() (string, error) { return "/tmp/otto-test-dummy", nil },
		httpClient:     &http.Client{Timeout: 2 * time.Second},
	}
	u.pending = &pendingUpdate{
		Tag:       "v1.0.1",
		AssetName: "otto-" + runtime.GOOS + "-" + runtime.GOARCH,
		AssetURL:  "https://otto-test-invalid.invalid/asset", // DNS will fail
	}
	h := &handler{updater: u, otto: newOttoState(), bot: bot}
	return h, bot
}

func TestUpdateCommandPending(t *testing.T) {
	h, _ := newPendingHandler(t)
	got := h.tryCommand(context.Background(), telegram.Update{Text: "/update"})
	if !got.handled {
		t.Fatal("not handled")
	}
	if !strings.Contains(got.reply, "v1.0.1") {
		t.Errorf("reply missing target tag: %q", got.reply)
	}
	if !strings.Contains(got.reply, "Starting update") {
		t.Errorf("reply=%q", got.reply)
	}
}

func TestUpdateCommandPendingOttoBusy(t *testing.T) {
	h, _ := newPendingHandler(t)
	h.otto.tryAcquire("doing a thing")

	got := h.tryCommand(context.Background(), telegram.Update{Text: "/update"})
	if !strings.Contains(got.reply, "interrupted") {
		t.Errorf("expected busy warning, got %q", got.reply)
	}
}
