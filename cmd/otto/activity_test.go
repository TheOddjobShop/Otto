//go:build unix

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/store"
	"otto/internal/telegram"
)

func TestSummarizeToolInput(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash takes the command", "Bash", `{"command":"npm test","description":"run tests"}`, "npm test"},
		{"read takes the path", "Read", `{"file_path":"/src/auth.ts","limit":50}`, "/src/auth.ts"},
		{"edit takes the path", "Edit", `{"file_path":"/src/a.go","old_string":"x","new_string":"y"}`, "/src/a.go"},
		{"grep takes the pattern", "Grep", `{"pattern":"func main","path":"."}`, "func main"},
		{"multiline collapses to one line", "Bash", `{"command":"go build ./...\n  && go test"}`, "go build ./... && go test"},
		{"unknown tool falls back to first string arg by sorted key", "mcp__gmail__search", `{"query":"from:boss","zzz":"ignored"}`, "query=from:boss"},
		{"no string args yields empty", "Weird", `{"n":1,"ok":true}`, ""},
		{"malformed json yields empty", "Bash", `not json`, ""},
		{"empty input yields empty", "Bash", ``, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := summarizeToolInput(c.tool, json.RawMessage(c.input))
			if got != c.want {
				t.Errorf("summarizeToolInput(%q, %s) = %q, want %q", c.tool, c.input, got, c.want)
			}
		})
	}
}

func TestSummarizeToolInputIsCapped(t *testing.T) {
	long := strings.Repeat("a", activityDetailCap*2)
	got := summarizeToolInput("Bash", json.RawMessage(`{"command":"`+long+`"}`))
	if len([]rune(got)) > activityDetailCap+1 {
		t.Errorf("not capped: %d runes", len([]rune(got)))
	}
}

// TestTurnKeysAreUnique — keys group activity rows into turns, so a collision
// would splice two unrelated turns together in any query.
func TestTurnKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		k := newTurnKey()
		if seen[k] {
			t.Fatalf("duplicate turn key %q at iteration %d", k, i)
		}
		seen[k] = true
	}
}

func TestActivityRingIsBounded(t *testing.T) {
	s := newOttoState()
	s.tryAcquire("work")
	for i := 0; i < activityRingCap*3; i++ {
		s.recordToolUse("id", "Bash", "cmd")
	}
	if got := len(s.Snapshot().Activity); got != activityRingCap {
		t.Errorf("ring holds %d entries, want cap of %d", got, activityRingCap)
	}
}

// TestActivityClearedOnRelease — the ring answers "what is Otto doing RIGHT
// NOW". Carrying a finished turn's tool calls forward would reintroduce
// exactly the stale-answer problem the feature exists to fix.
func TestActivityClearedOnRelease(t *testing.T) {
	s := newOttoState()
	s.tryAcquire("work")
	s.recordToolUse("t1", "Bash", "npm test")
	if len(s.Snapshot().Activity) == 0 {
		t.Fatal("activity not recorded while busy")
	}
	s.release()
	if got := s.Snapshot().Activity; len(got) != 0 {
		t.Errorf("activity survived release: %+v", got)
	}
	if s.currentTurnKey() != "" {
		t.Error("turn key survived release")
	}
}

// TestRecordWithoutTurnIsDropped — activity outside a turn has no turn key, so
// it could never be queried back; recording it would just be noise.
func TestRecordWithoutTurnIsDropped(t *testing.T) {
	s := newOttoState()
	if tk := s.recordToolUse("t1", "Bash", "x"); tk != "" {
		t.Errorf("recordToolUse outside a turn returned key %q", tk)
	}
	if _, _, ok := s.recordToolResult("t1", true, "boom"); ok {
		t.Error("recordToolResult outside a turn reported ok")
	}
}

// TestToolResultAttribution — results carry only a tool_use_id, so the tool
// name has to be correlated from the earlier call or the log line is anonymous.
func TestToolResultAttribution(t *testing.T) {
	s := newOttoState()
	s.tryAcquire("work")
	s.recordToolUse("toolu_9", "Bash", "npm test")
	_, tool, ok := s.recordToolResult("toolu_9", true, "2 failed")
	if !ok || tool != "Bash" {
		t.Fatalf("attribution failed: tool=%q ok=%v", tool, ok)
	}
	// Only failures enter the ring; the tool call plus the failure = 2.
	if got := len(s.Snapshot().Activity); got != 2 {
		t.Errorf("ring has %d entries, want 2 (call + failure)", got)
	}
}

func TestSuccessfulResultsStayOutOfTheRing(t *testing.T) {
	s := newOttoState()
	s.tryAcquire("work")
	s.recordToolUse("t1", "Read", "/a.go")
	s.recordToolResult("t1", false, "file contents")
	if got := len(s.Snapshot().Activity); got != 1 {
		t.Errorf("ring has %d entries, want 1 (the call only)", got)
	}
}

func TestFormatActivityForPet(t *testing.T) {
	now := time.Now()
	out := formatActivityForPet([]activityEntry{
		{At: now, Kind: store.ActivityTurnStart, Detail: "fix the auth bug"},
		{At: now, Kind: store.ActivityTool, Tool: "Bash", Detail: "npm test"},
		{At: now, Kind: store.ActivityResult, Tool: "Bash", Detail: "2 failed", IsError: true},
		{At: now, Kind: store.ActivityResult, Tool: "Read", Detail: "ok", IsError: false},
	})
	for _, want := range []string{"fix the auth bug", "Bash", "npm test", "failed: 2 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "ok") && strings.Contains(out, "Read") {
		t.Errorf("successful result should not be rendered:\n%s", out)
	}
	if got := formatActivityForPet(nil); got != "" {
		t.Errorf("empty activity should render nothing, got %q", got)
	}
}

// toolEmittingRunner emits a tool_use + failing tool_result, then blocks so a
// test can observe Otto mid-turn.
type toolEmittingRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *toolEmittingRunner) Run(ctx context.Context, args claude.RunArgs) error {
	args.Events <- claude.ToolUseEvent{ID: "t1", Name: "Bash", Input: json.RawMessage(`{"command":"npm test"}`)}
	args.Events <- claude.ToolResultEvent{ToolUseID: "t1", IsError: true, Content: "2 tests failed"}
	args.Events <- claude.ToolUseEvent{ID: "t2", Name: "Read", Input: json.RawMessage(`{"file_path":"/src/auth.ts"}`)}
	r.once.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	args.Events <- claude.ResultEvent{Subtype: "success"}
	return nil
}

func (r *toolEmittingRunner) WithEnv(map[string]string) claude.Runner { return r }

// TestTotoSeesOttosToolCalls is the whole point of the feature: while Otto is
// mid-turn running tools and saying nothing, Toto's prompt must describe what
// he is actually doing.
func TestTotoSeesOttosToolCalls(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ottoRunner := &toolEmittingRunner{started: make(chan struct{}), release: make(chan struct{})}
	totoRunner := &recordingRunner{fired: make(chan struct{})}

	sess, err := claude.LoadSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	totoSess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	bot := &fakeBot{updates: [][]telegram.Update{{
		{UpdateID: 1, UserID: 99, ChatID: 99, Text: "fix the failing tests"},
		{UpdateID: 2, UserID: 99, ChatID: 99, Text: "what's going on?"},
	}}}
	toto := &Toto{bot: bot, runner: totoRunner, session: totoSess, persona: "cat"}
	h := &handler{
		bot: bot, allow: auth.New(99), session: sess, runner: ottoRunner,
		startedAt: time.Now(), otto: newOttoState(), toto: toto,
		pets: newPetRegistry(toto), store: st,
	}
	toto.ottoStatus = h.otto.Snapshot

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = h.runPollingLoop(ctx) }()

	select {
	case <-ottoRunner.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Otto never started")
	}
	select {
	case <-totoRunner.fired:
	case <-time.After(5 * time.Second):
		t.Fatal("Toto never replied")
	}

	prompt := totoRunner.first().AppendSystemPrompt
	for _, want := range []string{
		"WHAT OTTO IS ACTUALLY DOING",
		"Bash", "npm test",
		"failed: 2 tests failed",
		"Read", "/src/auth.ts",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("Toto's prompt missing %q:\n%s", want, prompt)
		}
	}

	// Let Otto's turn run to completion (which writes the turn_end bookend)
	// before tearing down, so the durable assertions below aren't racing it.
	close(ottoRunner.release)
	cancel()
	h.WaitDispatches()

	// And it landed durably, not only in the ring.
	rows, err := st.RecentActivity(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var sawTool, sawErr, sawEnd bool
	for _, r := range rows {
		switch {
		case r.Kind == store.ActivityTool && r.Tool == "Bash":
			sawTool = true
		case r.Kind == store.ActivityResult && r.IsError:
			sawErr = true
		case r.Kind == store.ActivityTurnEnd:
			sawEnd = true
		}
	}
	if !sawTool || !sawErr || !sawEnd {
		t.Errorf("activity table incomplete: tool=%v err=%v end=%v (%d rows)", sawTool, sawErr, sawEnd, len(rows))
	}
}
