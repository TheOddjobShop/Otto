//go:build unix

package main

import (
	"context"
	"testing"

	"otto/internal/telegram"
)

func TestParseModelFromVerdict(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"CODE", ottoCodingModel},
		{"CHAT", ottoDefaultModel},
		{"  code  ", ottoCodingModel},
		{"code\n", ottoCodingModel},
		{"CODE.", ottoCodingModel},
		{"Code", ottoCodingModel},
		{"chat", ottoDefaultModel},
		{"", ottoDefaultModel},
		{"   ", ottoDefaultModel},
		{"banana", ottoDefaultModel},
		// Leading prose should not accidentally escalate: first token wins.
		{"I think this is CHAT", ottoDefaultModel},
		{"This is CODE work", ottoDefaultModel},
	}
	for _, c := range cases {
		if got := parseModelFromVerdict(c.raw); got != c.want {
			t.Errorf("parseModelFromVerdict(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

func TestModelLabel(t *testing.T) {
	cases := map[string]string{
		ottoCodingModel:  "opus-4.8 (coding)",
		ottoDefaultModel: "sonnet-4.6 (chat)",
		"":               "default (inherited)",
		"claude-x":       "claude-x",
	}
	for in, want := range cases {
		if got := modelLabel(in); got != want {
			t.Errorf("modelLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeClassifier returns a fixed model and records the message it saw.
type fakeClassifier struct {
	model string
	seen  []string
}

func (f *fakeClassifier) classify(ctx context.Context, message string) string {
	f.seen = append(f.seen, message)
	return f.model
}

func TestHandlerRoutesCodingTurnToOpus(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "fix the bug in runner.go"}}},
	}
	runner := &fakeRunner{respond: "on it"}
	h := newTestHandler(t, bot, runner)
	clf := &fakeClassifier{model: ottoCodingModel}
	h.classifier = clf
	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if runner.called[0].Model != ottoCodingModel {
		t.Errorf("Model = %q, want %q", runner.called[0].Model, ottoCodingModel)
	}
	if h.otto.lastModel != ottoCodingModel {
		t.Errorf("lastModel = %q, want %q", h.otto.lastModel, ottoCodingModel)
	}
	if len(clf.seen) != 1 || clf.seen[0] != "fix the bug in runner.go" {
		t.Errorf("classifier saw %v", clf.seen)
	}
}

func TestHandlerRoutesChatTurnToSonnet(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "what's 10 million won in usd"}}},
	}
	runner := &fakeRunner{respond: "about..."}
	h := newTestHandler(t, bot, runner)
	h.classifier = &fakeClassifier{model: ottoDefaultModel}
	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if runner.called[0].Model != ottoDefaultModel {
		t.Errorf("Model = %q, want %q", runner.called[0].Model, ottoDefaultModel)
	}
}

func TestHandlerNoClassifierInheritsDefaultModel(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{respond: "hello!"}
	h := newTestHandler(t, bot, runner) // classifier left nil
	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if runner.called[0].Model != "" {
		t.Errorf("Model = %q, want empty (inherited) when no classifier", runner.called[0].Model)
	}
}
