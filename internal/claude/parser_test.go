package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseStreamAccumulatesAssistantText(t *testing.T) {
	// Two assistant text deltas, then a result event.
	in := strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello "}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"world."}]}}
{"type":"result","subtype":"success"}
`)

	events := make(chan Event, 16)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)

	var assistantText strings.Builder
	sawResult := false
	for ev := range events {
		switch e := ev.(type) {
		case AssistantTextEvent:
			assistantText.WriteString(e.Text)
		case ResultEvent:
			sawResult = true
		}
	}
	if got := assistantText.String(); got != "Hello world." {
		t.Errorf("assistant text = %q, want %q", got, "Hello world.")
	}
	if !sawResult {
		t.Error("did not see ResultEvent")
	}
}

func TestParseStreamIgnoresUnknownTypes(t *testing.T) {
	in := strings.NewReader(`
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
`)
	events := make(chan Event, 8)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	var saw []Event
	for ev := range events {
		saw = append(saw, ev)
	}
	if len(saw) != 1 {
		t.Errorf("got %d events, want 1 (only assistant text)", len(saw))
	}
}

func TestParseStreamSkipsBlankLines(t *testing.T) {
	in := strings.NewReader("\n\n\n")
	events := make(chan Event, 1)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	for range events {
		t.Fatal("expected no events")
	}
}

func TestParseStreamEmitsSessionEvent(t *testing.T) {
	in := strings.NewReader(`{"type":"system","subtype":"init","session_id":"sess-abc-123","cwd":"/tmp","tools":[]}` + "\n" +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}` + "\n")
	events := make(chan Event, 8)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	var sawSession SessionEvent
	for ev := range events {
		if e, ok := ev.(SessionEvent); ok {
			sawSession = e
		}
	}
	if sawSession.ID != "sess-abc-123" {
		t.Errorf("SessionEvent.ID = %q, want sess-abc-123", sawSession.ID)
	}
}

func TestParseStreamSystemInitWithoutSessionIDIgnored(t *testing.T) {
	in := strings.NewReader(`{"type":"system","subtype":"init"}` + "\n")
	events := make(chan Event, 4)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	for range events {
		t.Fatal("expected no events for system/init without session_id")
	}
}

func TestParseStreamSurfacesPermissionDenials(t *testing.T) {
	in := strings.NewReader(`{"type":"result","subtype":"success","permission_denials":[{"tool_name":"mcp__gmail-personal__search_emails","tool_use_id":"toolu_abc"},{"tool_name":"Bash","tool_use_id":"toolu_def"}]}` + "\n")
	events := make(chan Event, 4)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	var got ResultEvent
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got = r
		}
	}
	if len(got.PermissionDenials) != 2 {
		t.Fatalf("got %d denials, want 2", len(got.PermissionDenials))
	}
	if got.PermissionDenials[0].ToolName != "mcp__gmail-personal__search_emails" {
		t.Errorf("denial[0] tool = %q", got.PermissionDenials[0].ToolName)
	}
	if got.PermissionDenials[1].ToolName != "Bash" {
		t.Errorf("denial[1] tool = %q", got.PermissionDenials[1].ToolName)
	}
}

// A single malformed/non-JSON line (e.g. stray stdout) must be skipped rather
// than aborting the whole stream and dropping the final result event.
func TestParseStreamMalformedLineSkipped(t *testing.T) {
	in := strings.NewReader("{not valid json\n" +
		`{"type":"result","subtype":"success"}` + "\n")
	events := make(chan Event, 4)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	sawResult := false
	for ev := range events {
		if _, ok := ev.(ResultEvent); ok {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatal("expected ResultEvent after skipping malformed line")
	}
}

// On a non-success result frame with no top-level "error" field, the result
// body ("result") is surfaced as ResultEvent.Error so the diagnostic isn't lost.
func TestParseStreamResultErrorFallback(t *testing.T) {
	in := strings.NewReader(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"tool execution failed"}` + "\n")
	events := make(chan Event, 4)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	var got ResultEvent
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got = r
		}
	}
	if got.Error != "tool execution failed" {
		t.Fatalf("ResultEvent.Error = %q, want %q", got.Error, "tool execution failed")
	}
}

func TestParseStreamResultUsageFields(t *testing.T) {
	line := `{"type":"result","subtype":"success","usage":{"input_tokens":7,"output_tokens":42,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000}}` + "\n"

	events := make(chan Event, 4)
	if err := ParseStream(context.Background(), strings.NewReader(line), events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)

	var got ResultEvent
	var found bool
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got, found = r, true
		}
	}
	if !found {
		t.Fatal("no ResultEvent emitted")
	}
	if got.InputTokens != 7 || got.OutputTokens != 42 {
		t.Errorf("in/out = %d/%d, want 7/42", got.InputTokens, got.OutputTokens)
	}
	if got.CacheCreationTokens != 100 || got.CacheReadTokens != 2000 {
		t.Errorf("cache = %d/%d, want 100/2000", got.CacheCreationTokens, got.CacheReadTokens)
	}
	// Regression: ContextTokens still sums the three input-side fields.
	if got.ContextTokens != 7+100+2000 {
		t.Errorf("ContextTokens = %d, want %d", got.ContextTokens, 7+100+2000)
	}
}

func TestParseStreamReturnsOnContextCancel(t *testing.T) {
	// Unbuffered channel — first send will block until cancellation lands.
	events := make(chan Event)
	in := strings.NewReader(`{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}` + "\n")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ParseStream(ctx, in, events) }()

	// Don't drain; cancel before reader could possibly receive.
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from cancelled ParseStream")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ParseStream did not return after ctx cancel — deadlock")
	}
}

func TestParseStreamCapturesContextTokens(t *testing.T) {
	line := `{"type":"result","subtype":"success","usage":{"input_tokens":4242,"output_tokens":17},"session_id":"s1"}` + "\n"
	events := make(chan Event, 8)
	go func() {
		_ = ParseStream(context.Background(), strings.NewReader(line), events)
		close(events)
	}()
	var got ResultEvent
	var found bool
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got = r
			found = true
		}
	}
	if !found {
		t.Fatal("no ResultEvent emitted")
	}
	if got.ContextTokens != 4242 {
		t.Fatalf("ContextTokens = %d, want 4242", got.ContextTokens)
	}
}

// TestParseStreamSumsCacheTokens is the regression guard for the rotator
// blindspot: under Claude Code prompt caching the bulk of the live context
// is reported in cache_read_input_tokens (and cache_creation_input_tokens),
// while input_tokens is just the uncached delta (often single digits). The
// session rotator measures ContextTokens to decide when to clear, so it MUST
// reflect total occupancy, not the tiny delta — otherwise a 118k-token
// session reads as ~2 tokens and never rotates.
func TestParseStreamSumsCacheTokens(t *testing.T) {
	// Real shape captured from a live Otto turn: 2 + 1836 + 115867 = 117705.
	line := `{"type":"result","subtype":"success","usage":{"input_tokens":2,"cache_creation_input_tokens":1836,"cache_read_input_tokens":115867,"output_tokens":1049},"session_id":"s1"}` + "\n"
	events := make(chan Event, 8)
	go func() {
		_ = ParseStream(context.Background(), strings.NewReader(line), events)
		close(events)
	}()
	var got ResultEvent
	var found bool
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got = r
			found = true
		}
	}
	if !found {
		t.Fatal("no ResultEvent emitted")
	}
	if got.ContextTokens != 117705 {
		t.Fatalf("ContextTokens = %d, want 117705 (input+cache_creation+cache_read)", got.ContextTokens)
	}
}
