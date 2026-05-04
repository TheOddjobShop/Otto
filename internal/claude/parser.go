package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// rawMessage matches the subset of stream-json events we use.
type rawMessage struct {
	Type              string              `json:"type"`
	Subtype           string              `json:"subtype"`
	SessionID         string              `json:"session_id"`
	Error             string              `json:"error"`
	PermissionDenials []rawPermissionDeny `json:"permission_denials"`
	Message           struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

type rawPermissionDeny struct {
	ToolName  string `json:"tool_name"`
	ToolUseID string `json:"tool_use_id"`
}

// ParseStream reads newline-delimited JSON from r and forwards interpreted
// events to events. Returns on EOF, ctx cancel, or first parse error.
func ParseStream(ctx context.Context, r io.Reader, events chan<- Event) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw rawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			return fmt.Errorf("claude: parse stream-json: %w", err)
		}
		switch raw.Type {
		case "system":
			if raw.Subtype == "init" && raw.SessionID != "" {
				select {
				case events <- SessionEvent{ID: raw.SessionID}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		case "assistant":
			for _, c := range raw.Message.Content {
				if c.Type == "text" && c.Text != "" {
					select {
					case events <- AssistantTextEvent{Text: c.Text}:
					case <-ctx.Done():
						return ctx.Err()
					}
				}
			}
		case "result":
			ev := ResultEvent{Subtype: raw.Subtype, Error: raw.Error}
			for _, d := range raw.PermissionDenials {
				if d.ToolName == "" {
					continue
				}
				ev.PermissionDenials = append(ev.PermissionDenials, PermissionDenial{
					ToolName:  d.ToolName,
					ToolUseID: d.ToolUseID,
				})
			}
			select {
			case events <- ev:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("claude: scan stream: %w", err)
	}
	return nil
}
