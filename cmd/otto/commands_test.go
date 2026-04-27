//go:build unix

// This file is in the same package as handler_test.go and reuses fakeBot,
// fakeRunner, newTestHandler, and runForBriefWindow from there.

package main

import (
	"strings"
	"testing"

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
