//go:build unix

package main

import (
	"context"
	"strings"
	"testing"
)

func TestTootSendIncludesArtAndBody(t *testing.T) {
	bot := &fakeBot{}
	toot := newToot(bot)
	if err := toot.Send(context.Background(), 42, "v1.0.1 is out"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if !strings.Contains(msg, "<pre>") {
		t.Errorf("missing <pre> wrapper: %q", msg)
	}
	if !strings.Contains(msg, "v1.0.1 is out") {
		t.Errorf("missing body: %q", msg)
	}
	// All three owl arts include "(o,o)" — verify one was selected.
	if !strings.Contains(msg, "(o,o)") {
		t.Errorf("missing owl signature: %q", msg)
	}
}

func TestTootSendEscapesHTMLInBody(t *testing.T) {
	bot := &fakeBot{}
	toot := newToot(bot)
	if err := toot.Send(context.Background(), 42, "<script>alert(1)</script>"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bot.sent[0].text, "&lt;script&gt;") {
		t.Errorf("expected HTML-escaped body, got %q", bot.sent[0].text)
	}
}
