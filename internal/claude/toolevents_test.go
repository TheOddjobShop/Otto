package claude

import (
	"context"
	"strings"
	"testing"
)

// collect drains ParseStream over a canned stream and returns the events.
func collect(t *testing.T, stream string) []Event {
	t.Helper()
	ch := make(chan Event, 64)
	done := make(chan []Event)
	go func() {
		var out []Event
		for ev := range ch {
			out = append(out, ev)
		}
		done <- out
	}()
	if err := ParseStream(context.Background(), strings.NewReader(stream), ch); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(ch)
	return <-done
}

// TestParseToolUse is the foundation of the activity log: before this, tool_use
// blocks were silently dropped and Otto's actions were invisible.
func TestParseToolUse(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"text","text":"Let me check."},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"npm test"}}]}}
`
	evs := collect(t, stream)
	if len(evs) != 2 {
		t.Fatalf("got %d events, want 2 (text + tool_use): %#v", len(evs), evs)
	}
	if _, ok := evs[0].(AssistantTextEvent); !ok {
		t.Errorf("event 0 = %T, want AssistantTextEvent", evs[0])
	}
	tu, ok := evs[1].(ToolUseEvent)
	if !ok {
		t.Fatalf("event 1 = %T, want ToolUseEvent", evs[1])
	}
	if tu.ID != "toolu_1" || tu.Name != "Bash" {
		t.Errorf("got id=%q name=%q", tu.ID, tu.Name)
	}
	if !strings.Contains(string(tu.Input), "npm test") {
		t.Errorf("input not carried through: %s", tu.Input)
	}
}

// TestParseToolResultBothShapes — the API types tool_result content as
// `string | ContentBlock[]`, and both shapes appear in practice.
func TestParseToolResultBothShapes(t *testing.T) {
	t.Run("string content", func(t *testing.T) {
		stream := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"2 tests failed","is_error":true}]}}
`
		evs := collect(t, stream)
		if len(evs) != 1 {
			t.Fatalf("got %d events, want 1", len(evs))
		}
		tr := evs[0].(ToolResultEvent)
		if tr.ToolUseID != "toolu_1" || !tr.IsError || tr.Content != "2 tests failed" {
			t.Errorf("got %+v", tr)
		}
	})

	t.Run("block-array content", func(t *testing.T) {
		stream := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"line one"},{"type":"text","text":"line two"}]}]}}
`
		evs := collect(t, stream)
		tr := evs[0].(ToolResultEvent)
		if tr.Content != "line one\nline two" {
			t.Errorf("content = %q", tr.Content)
		}
		if tr.IsError {
			t.Error("IsError should default false")
		}
	})

	t.Run("non-text content yields empty, not a crash", func(t *testing.T) {
		stream := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{}}]}]}}
`
		if got := collect(t, stream)[0].(ToolResultEvent).Content; got != "" {
			t.Errorf("content = %q, want empty", got)
		}
	})
}

// TestToolResultContentIsCapped — a tool result can be megabytes; the event
// feeds a one-line log entry, so the parser must not copy it all through.
func TestToolResultContentIsCapped(t *testing.T) {
	huge := strings.Repeat("x", toolResultTextCap*3)
	stream := `{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t","content":"` + huge + `"}]}}
`
	got := collect(t, stream)[0].(ToolResultEvent).Content
	if len([]rune(got)) > toolResultTextCap+1 { // +1 for the ellipsis
		t.Errorf("content not capped: %d runes", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Error("truncated content should be marked with an ellipsis")
	}
}

// TestUnknownBlockTypesAreInert guards forward-compatibility: a new content
// block type upstream must be skipped, not fatal.
func TestUnknownBlockTypesAreInert(t *testing.T) {
	stream := `{"type":"assistant","message":{"content":[{"type":"some_future_block","text":"x"},{"type":"text","text":"real"}]}}
`
	evs := collect(t, stream)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want just the text one", len(evs))
	}
	if evs[0].(AssistantTextEvent).Text != "real" {
		t.Errorf("got %+v", evs[0])
	}
}
