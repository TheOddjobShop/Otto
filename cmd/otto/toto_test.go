//go:build unix

package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/claude"
	"otto/internal/memory"
	"otto/internal/store"
)

func TestTotoInjectsMemoryAndLogsTurn(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{respond: "mrow"}
	dir := t.TempDir()
	core := memory.NewCore(dir, 2200, 1375)
	if err := core.Add(memory.TargetUser, "User prefers brevity."); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	toto := &Toto{
		bot:     bot,
		runner:  runner,
		session: sess,
		persona: "CAT PERSONA",
		mem:     core,
		store:   st,
	}

	toto.Reply(context.Background(), 100, "hello")

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "User prefers brevity") {
		t.Errorf("toto prompt missing injected memory: %q", runner.called[0].AppendSystemPrompt)
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "CURRENT TIME") {
		t.Errorf("toto prompt missing time block: %q", runner.called[0].AppendSystemPrompt)
	}
	if got, _ := st.SearchFTS(context.Background(), "hello", 5); len(got) == 0 {
		t.Error("toto user turn not logged")
	}
}

// newTestToto builds a Toto wired to fakes, with no store/memory.
func newTestToto(t *testing.T, runner *fakeRunner) *Toto {
	t.Helper()
	sess, err := claude.LoadSession(filepath.Join(t.TempDir(), "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	return &Toto{
		bot:     &fakeBot{},
		runner:  runner,
		session: sess,
		persona: "CAT PERSONA",
	}
}

// TestTotoLiveStatusOnUserPrompt pins the core of the visibility fix: the
// VOLATILE Otto status (busy/idle, his prompt, the reply tail) must ride on
// the USER-side prompt, not --append-system-prompt.
//
// Toto runs with --resume. A small model weighs its own prior assistant turn
// over a system prompt that changed silently between turns, so when status
// lived in the system prompt Toto answered "what's he doing now?" by echoing
// his previous "idle. nothing." Inline status makes each turn's history carry
// the state it was answered against.
func TestTotoLiveStatusOnUserPrompt(t *testing.T) {
	t.Run("direct-address-idle", func(t *testing.T) {
		runner := &fakeRunner{respond: "mrow"}
		toto := newTestToto(t, runner)
		toto.ottoStatus = func() ottoSnapshot { return ottoSnapshot{Busy: false} }

		toto.Reply(context.Background(), 100, "toto what's otto doing")

		if len(runner.called) != 1 {
			t.Fatalf("runner called %d times, want 1", len(runner.called))
		}
		got := runner.called[0]
		if !strings.HasPrefix(got.Prompt, "(otto status: idle)") {
			t.Errorf("user prompt missing idle note, got %q", got.Prompt)
		}
		if !strings.Contains(got.Prompt, "toto what's otto doing") {
			t.Errorf("user prompt lost the user's message: %q", got.Prompt)
		}
		if strings.Contains(got.AppendSystemPrompt, "(otto status:") {
			t.Errorf("live status leaked into the system prompt:\n%s", got.AppendSystemPrompt)
		}
		// The standing rule (not the data) stays in the system prompt.
		if !strings.Contains(got.AppendSystemPrompt, "re-read it every turn") {
			t.Errorf("system prompt missing the re-read guardrail:\n%s", got.AppendSystemPrompt)
		}
	})

	t.Run("direct-address-busy-with-snippet-and-silence", func(t *testing.T) {
		runner := &fakeRunner{respond: "mrow"}
		toto := newTestToto(t, runner)
		toto.ottoStatus = func() ottoSnapshot {
			return ottoSnapshot{
				Busy:          true,
				CurrentPrompt: "summarize my inbox",
				Snippet:       "scanning gmail...",
				Silence:       90 * time.Second,
			}
		}

		toto.Reply(context.Background(), 100, "what's he doing now?")

		got := runner.called[0].Prompt
		for _, want := range []string{
			`(otto status: busy on "summarize my inbox")`,
			`(tail of his reply: "scanning gmail...")`,
			"(he's been silent for 1m30s)",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("user prompt missing %q, got:\n%s", want, got)
			}
		}
	})

	t.Run("direct-address-no-status-wired", func(t *testing.T) {
		runner := &fakeRunner{respond: "mrow"}
		toto := newTestToto(t, runner) // ottoStatus stays nil

		toto.Reply(context.Background(), 100, "hi")

		if got := runner.called[0].Prompt; got != "hi" {
			t.Errorf("prompt = %q, want the bare message when no status is wired", got)
		}
	})

	t.Run("bare-ping-keeps-status-and-fallback-body", func(t *testing.T) {
		runner := &fakeRunner{respond: "mrow"}
		toto := newTestToto(t, runner)
		toto.ottoStatus = func() ottoSnapshot { return ottoSnapshot{Busy: false} }

		toto.Reply(context.Background(), 100, "")

		got := runner.called[0].Prompt
		if !strings.HasPrefix(got, "(otto status: idle)") {
			t.Errorf("bare ping lost the status note: %q", got)
		}
		if !strings.Contains(got, "pinged you with no content") {
			t.Errorf("bare ping lost the empty-body fallback: %q", got)
		}
	})

	t.Run("busy-fallback", func(t *testing.T) {
		runner := &fakeRunner{respond: "mrow"}
		toto := newTestToto(t, runner)

		toto.BusyReply(context.Background(), 100, "hey", "summarize my inbox", "scanning gmail...", nil)

		got := runner.called[0]
		if !strings.Contains(got.Prompt, `(otto status: busy on "summarize my inbox")`) {
			t.Errorf("busy note missing from user prompt: %q", got.Prompt)
		}
		if !strings.Contains(got.Prompt, `(tail of his reply: "scanning gmail...")`) {
			t.Errorf("reply tail missing from user prompt: %q", got.Prompt)
		}
		if strings.Contains(got.AppendSystemPrompt, "summarize my inbox") {
			t.Errorf("Otto's prompt leaked into the system prompt:\n%s", got.AppendSystemPrompt)
		}
	})

	t.Run("busy-fallback-no-snippet", func(t *testing.T) {
		runner := &fakeRunner{respond: "mrow"}
		toto := newTestToto(t, runner)

		toto.BusyReply(context.Background(), 100, "hey", "summarize my inbox", "", nil)

		got := runner.called[0].Prompt
		if strings.Contains(got, "tail of his reply") {
			t.Errorf("empty snippet should not emit a tail line: %q", got)
		}
	})
}

// TestOttoStatusNote covers the note builder's shaping rules directly:
// multi-line values collapse to one line, over-long values truncate, and a
// busy-with-no-prompt state still reports busy.
func TestOttoStatusNote(t *testing.T) {
	t.Run("collapses-newlines", func(t *testing.T) {
		got := ottoStatusNote(true, "line one\nline two", "", 0)
		if strings.Count(got, "\n") != 0 {
			t.Errorf("note should be one line, got %q", got)
		}
		if !strings.Contains(got, "line one line two") {
			t.Errorf("newline not collapsed to space: %q", got)
		}
	})

	t.Run("truncates-long-values", func(t *testing.T) {
		got := ottoStatusNote(true, strings.Repeat("x", statusNoteCap+50), "", 0)
		if !strings.Contains(got, "…") {
			t.Errorf("over-long prompt not truncated: %q", got)
		}
	})

	t.Run("busy-without-prompt", func(t *testing.T) {
		if got := ottoStatusNote(true, "", "", 0); got != "(otto status: busy)" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("silence-below-threshold-omitted", func(t *testing.T) {
		got := ottoStatusNote(true, "work", "", 5*time.Second)
		if strings.Contains(got, "silent") {
			t.Errorf("short silence should be omitted: %q", got)
		}
	})
}
