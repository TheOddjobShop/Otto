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

func TestParseStreamMalformedLineReturnsError(t *testing.T) {
	in := strings.NewReader(`{not valid json`)
	events := make(chan Event, 1)
	if err := ParseStream(context.Background(), in, events); err == nil {
		t.Fatal("expected error, got nil")
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
