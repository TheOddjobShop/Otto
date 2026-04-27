package claude

// Event is the discriminated union emitted by ParseStream.
type Event interface{ isEvent() }

type AssistantTextEvent struct{ Text string }

func (AssistantTextEvent) isEvent() {}

type ResultEvent struct {
	Subtype string
	Error   string // populated when Subtype != "success"
	// PermissionDenials lists tools claude tried to use but was denied.
	// This can be non-empty even on a success Subtype: claude often
	// recovers by asking the user for permission and continuing.
	PermissionDenials []PermissionDenial
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
