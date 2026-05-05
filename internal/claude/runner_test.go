//go:build unix

package claude

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestTailBufKeepsLastCapBytes(t *testing.T) {
	tb := newTailBuf(10)
	for i := 0; i < 5; i++ {
		tb.Write([]byte("0123456789"))
	}
	got := tb.String()
	if len(got) != 10 {
		t.Errorf("len = %d, want 10", len(got))
	}
	if got != "0123456789" {
		t.Errorf("got = %q, want last 10 bytes", got)
	}
}

func TestTailBufBelowCap(t *testing.T) {
	tb := newTailBuf(100)
	tb.Write([]byte("hello"))
	if got := tb.String(); got != "hello" {
		t.Errorf("got = %q, want hello", got)
	}
}

func TestBuildErrorInfoPrefersStderr(t *testing.T) {
	got := buildErrorInfo("stderr-msg", "stdout-tail", errors.New("parse"))
	if got != "stderr-msg" {
		t.Errorf("got %q, want stderr-msg", got)
	}
}

func TestBuildErrorInfoFallsBackToStdout(t *testing.T) {
	got := buildErrorInfo("", "stdout-tail", errors.New("parse"))
	if got != "stdout-tail" {
		t.Errorf("got %q, want stdout-tail", got)
	}
}

func TestBuildErrorInfoFallsBackToParseErr(t *testing.T) {
	got := buildErrorInfo("", "", errors.New("invalid json"))
	if !strings.Contains(got, "parser: invalid json") {
		t.Errorf("got %q, want contains parser: invalid json", got)
	}
}

func TestBuildErrorInfoEmptyAll(t *testing.T) {
	got := buildErrorInfo("", "", nil)
	if !strings.Contains(got, "journalctl") {
		t.Errorf("got %q, want hint to check journalctl", got)
	}
}

func TestBuildErrorInfoTruncates(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := buildErrorInfo("", long, nil)
	if len(got) > 1600 { // 1500 cap + ellipsis prefix
		t.Errorf("len = %d, want <= 1600", len(got))
	}
	if !strings.HasPrefix(got, "...\n") {
		t.Errorf("expected truncation prefix, got %q...", got[:20])
	}
}

func TestBuildCmdArgsAlwaysSkipsPermissions(t *testing.T) {
	// Otto runs in -p mode under a single-user allowlist; the permission
	// prompt has no interactive surface, so we always skip it. Test all
	// flag combinations.
	cases := []struct {
		name string
		got  []string
	}{
		{"empty", buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, nil, nil)},
		{"with-session", buildCmdArgs("hi", "abc", "/tmp/mcp.json", "", "", "", nil, nil, nil)},
		{"with-sysprompt", buildCmdArgs("hi", "", "/tmp/mcp.json", "be kind", "", "", nil, nil, nil)},
		{"with-images", buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", []string{"/tmp/a.png"}, nil, nil)},
		{"with-allowed", buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, []string{"mcp__gmail__*"}, nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !slices.Contains(tc.got, "--dangerously-skip-permissions") {
				t.Errorf("--dangerously-skip-permissions missing: %v", tc.got)
			}
		})
	}
}

func TestBuildCmdArgsOmitsResumeWhenSessionEmpty(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, nil, nil)
	if slices.Contains(got, "--resume") {
		t.Errorf("--resume should not appear with empty session: %v", got)
	}
}

func TestBuildCmdArgsIncludesResumeWhenSessionSet(t *testing.T) {
	got := buildCmdArgs("hi", "sess-abc", "/tmp/mcp.json", "", "", "", nil, nil, nil)
	idx := slices.Index(got, "--resume")
	if idx < 0 {
		t.Fatalf("--resume missing: %v", got)
	}
	if idx+1 >= len(got) || got[idx+1] != "sess-abc" {
		t.Errorf("--resume value wrong: %v", got)
	}
}

func TestBuildCmdArgsAppendsImagePathsToPrompt(t *testing.T) {
	got := buildCmdArgs("describe", "", "/tmp/mcp.json", "", "", "", []string{"/tmp/a.png", "/tmp/b.jpg"}, nil, nil)
	idx := slices.Index(got, "-p")
	if idx < 0 {
		t.Fatalf("-p missing: %v", got)
	}
	prompt := got[idx+1]
	if !strings.Contains(prompt, "@/tmp/a.png") || !strings.Contains(prompt, "@/tmp/b.jpg") {
		t.Errorf("image paths not appended: %q", prompt)
	}
}

func TestBuildCmdArgsOmitsSystemPromptWhenEmpty(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, nil, nil)
	if slices.Contains(got, "--append-system-prompt") {
		t.Errorf("--append-system-prompt should not appear when prompt empty: %v", got)
	}
}

func TestBuildCmdArgsIncludesSystemPromptWhenSet(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "be kind", "", "", nil, nil, nil)
	idx := slices.Index(got, "--append-system-prompt")
	if idx < 0 {
		t.Fatalf("--append-system-prompt missing: %v", got)
	}
	if idx+1 >= len(got) || got[idx+1] != "be kind" {
		t.Errorf("--append-system-prompt value wrong: %v", got)
	}
}

func TestBuildCmdArgsOmitsAllowedToolsWhenEmpty(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, nil, nil)
	if slices.Contains(got, "--allowed-tools") {
		t.Errorf("--allowed-tools should not appear when empty: %v", got)
	}
}

func TestBuildCmdArgsIncludesAllowedToolsAsCSV(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, []string{"mcp__gmail__*", "Bash"}, nil)
	idx := slices.Index(got, "--allowed-tools")
	if idx < 0 {
		t.Fatalf("--allowed-tools missing: %v", got)
	}
	if idx+1 >= len(got) || got[idx+1] != "mcp__gmail__*,Bash" {
		t.Errorf("--allowed-tools value = %q, want comma-separated", got[idx+1])
	}
}

func TestBuildCmdArgsIncludesEffortWhenSet(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "medium", nil, nil, nil)
	idx := slices.Index(got, "--effort")
	if idx < 0 {
		t.Fatalf("--effort missing: %v", got)
	}
	if idx+1 >= len(got) || got[idx+1] != "medium" {
		t.Errorf("--effort value = %q, want %q", got[idx+1], "medium")
	}
}

func TestBuildCmdArgsOmitsEffortWhenEmpty(t *testing.T) {
	got := buildCmdArgs("hi", "", "/tmp/mcp.json", "", "", "", nil, nil, nil)
	if slices.Contains(got, "--effort") {
		t.Errorf("--effort should not appear when empty: %v", got)
	}
}

func fakeClaudePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunnerStreamsAssistantText(t *testing.T) {
	r := NewExecRunner(fakeClaudePath(t), "/tmp/mcp.json", "", "")
	events := make(chan Event, 16)
	err := r.Run(context.Background(), RunArgs{
		Prompt:    "hi",
		SessionID: "abc",
		Events:    events,
	})
	close(events)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var text strings.Builder
	for ev := range events {
		if t, ok := ev.(AssistantTextEvent); ok {
			text.WriteString(t.Text)
		}
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q, want %q", text.String(), "hello world")
	}
}

func TestRunnerSurfacesNonZeroExit(t *testing.T) {
	r := NewExecRunner(fakeClaudePath(t), "/tmp/mcp.json", "", "")
	r = r.WithEnv(map[string]string{"FAKE_CLAUDE_MODE": "fail"})
	events := make(chan Event, 4)
	err := r.Run(context.Background(), RunArgs{Prompt: "hi", SessionID: "abc", Events: events})
	close(events)
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	if !strings.Contains(err.Error(), "fake error") {
		t.Errorf("error %q does not contain stderr", err)
	}
}

func TestRunnerRespectsContextTimeout(t *testing.T) {
	r := NewExecRunner(fakeClaudePath(t), "/tmp/mcp.json", "", "")
	r = r.WithEnv(map[string]string{"FAKE_CLAUDE_MODE": "hang"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	events := make(chan Event, 4)
	start := time.Now()
	err := r.Run(ctx, RunArgs{Prompt: "hi", SessionID: "abc", Events: events})
	close(events)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Run took too long: %v", time.Since(start))
	}
}
