//go:build unix

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"otto/internal/store"
)

func TestSayBodyFromArgs(t *testing.T) {
	got, err := sayBody([]string{"good", "morning"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "good morning" {
		t.Errorf("sayBody = %q, want the joined arguments", got)
	}
}

func TestSayBodyRejectsEmpty(t *testing.T) {
	if _, err := sayBody([]string{"   "}); err == nil {
		t.Error("a whitespace-only message should be rejected")
	}
}

func TestSayBodyRejectsOversized(t *testing.T) {
	huge := strings.Repeat("x", sayMaxBytes+1)
	if _, err := sayBody([]string{huge}); err == nil {
		t.Error("an oversized message should be rejected rather than queued")
	}
}

// The row shape matters: source="user" is what makes the dispatcher hand this
// straight to handleMessage with no BUS CONTEXT. Telling Otto he is mid-chain
// when a cron job pinged him would be false.
func TestSayEnqueuesUserSourcedRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if _, err := st.Enqueue(ctx, "otto", "user", "", "good morning", 0); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	msgs, err := st.DequeueAll(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("dequeued %d rows, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Source != "user" {
		t.Errorf("Source = %q, want user", m.Source)
	}
	if m.Sender != "" {
		t.Errorf("Sender = %q, want empty for a user-sourced row", m.Sender)
	}
	if m.Hop != 0 {
		t.Errorf("Hop = %d, want 0 — a scheduled message starts no chain", m.Hop)
	}
	if m.Body != "good morning" {
		t.Errorf("Body = %q", m.Body)
	}
}

func TestSayAcceptsEveryValidTarget(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, target := range []string{"otto", "toto", "toot"} {
		if _, err := st.Enqueue(ctx, target, "user", "", "ping", 0); err != nil {
			t.Errorf("enqueue to %s: %v", target, err)
		}
	}
	if _, err := st.Enqueue(ctx, "nobody", "user", "", "ping", 0); err == nil {
		t.Error("an unknown target should be refused")
	}
}

func TestSayUsageMentionsScheduledWork(t *testing.T) {
	for _, want := range []string{"launchd", "cron", "-to", "memory"} {
		if !strings.Contains(sayUsage, want) {
			t.Errorf("usage should mention %q so the intended use is discoverable", want)
		}
	}
}

// `otto say` must appear in the top-level help, or nothing will ever find it.
func TestSayIsDiscoverableFromHelp(t *testing.T) {
	if !strings.Contains(usageText, "otto say") {
		t.Error("otto say is missing from the top-level usage")
	}
}
