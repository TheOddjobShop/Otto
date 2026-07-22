package claude

import "encoding/json"

// Event is the discriminated union emitted by ParseStream.
type Event interface{ isEvent() }

type AssistantTextEvent struct{ Text string }

func (AssistantTextEvent) isEvent() {}

// ToolUseEvent is one tool invocation Claude made during a turn. Otto records
// these to the activity log so Toto can say what Otto is actually doing rather
// than only what he is saying — the assistant-text tail goes quiet for minutes
// during a long tool sequence, which is exactly when the user asks.
//
// Input is the raw JSON argument object, left unparsed here because its shape
// is per-tool; cmd/otto summarizes it for display.
type ToolUseEvent struct {
	ID    string
	Name  string
	Input json.RawMessage
}

func (ToolUseEvent) isEvent() {}

// ToolResultEvent is the outcome of a ToolUseEvent, correlated by ToolUseID.
// Claude Code reports these as `user`-role frames carrying tool_result blocks.
//
// Content is best-effort: the tool_result content field is either a plain
// string or an array of content blocks, so the parser flattens the text it can
// find and leaves the rest. It is only ever used for a truncated log line.
type ToolResultEvent struct {
	ToolUseID string
	IsError   bool
	Content   string
}

func (ToolResultEvent) isEvent() {}

type ResultEvent struct {
	Subtype string
	Error   string // populated when Subtype != "success"
	// PermissionDenials lists tools claude tried to use but was denied.
	// This can be non-empty even on a success Subtype: claude often
	// recovers by asking the user for permission and continuing.
	PermissionDenials []PermissionDenial
	// ContextTokens is the total input-side context occupancy for the turn:
	// usage.input_tokens + cache_creation_input_tokens + cache_read_input_tokens.
	// Summing the cache fields is essential — under prompt caching input_tokens
	// alone is just the uncached delta, so the session rotator that reads this
	// would otherwise never see the real context size. 0 if absent.
	ContextTokens int
	// Raw per-turn token counts from the result event's usage block, kept
	// separate from ContextTokens (which sums the input-side fields for the
	// rotator). These feed the token tracker. OutputTokens is not part of
	// ContextTokens — it is the generation cost, recorded for accounting only.
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

func (ResultEvent) isEvent() {}

// PermissionDenial is one denied tool call from a claude turn.
type PermissionDenial struct {
	ToolName  string
	ToolUseID string
}

// SessionEvent carries the session_id Claude Code reports via its
// system/init stream-json frame at the start of each invocation. Otto
// captures this from a no-resume invocation so subsequent invocations can
// pass --resume <id>.
type SessionEvent struct{ ID string }

func (SessionEvent) isEvent() {}
