package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
)

// rawMessage matches the subset of stream-json events we use.
type rawMessage struct {
	Type              string              `json:"type"`
	Subtype           string              `json:"subtype"`
	SessionID         string              `json:"session_id"`
	Error             string              `json:"error"`
	Result            string              `json:"result"`
	PermissionDenials []rawPermissionDeny `json:"permission_denials"`
	// Usage carries the result event's token accounting. Under prompt
	// caching, input_tokens is only the uncached delta (often single
	// digits) — the bulk of the live context is in the two cache fields.
	// The rotator needs total occupancy, so all three are summed into
	// ResultEvent.ContextTokens; reading input_tokens alone made the
	// rotator blind and the session never cleared.
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Message struct {
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
// events to events. Returns on EOF or ctx cancel, or on a reader-level error.
// Individual unparseable lines are logged and skipped, not returned as errors.
func ParseStream(ctx context.Context, r io.Reader, events chan<- Event) error {
	// bufio.Reader (vs bufio.Scanner) imposes no fixed per-line ceiling, so an
	// oversized-but-valid stream-json frame (large tool result or assistant
	// text) no longer aborts the turn with bufio.ErrTooLong.
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			if len(trimmed) > 0 {
				var raw rawMessage
				if uerr := json.Unmarshal(trimmed, &raw); uerr != nil {
					// A single malformed/non-JSON line (e.g. stray stdout from
					// a hook or a partial line) must not abort the whole stream
					// and drop the final result event. Skip and continue;
					// reserve hard errors for reader-level failures below.
					log.Printf("claude: skipping unparseable stream-json line: %v", uerr)
				} else {
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
						ctxTokens := raw.Usage.InputTokens + raw.Usage.CacheCreationInputTokens + raw.Usage.CacheReadInputTokens
						errMsg := raw.Error
						if errMsg == "" && raw.Subtype != "success" {
							errMsg = raw.Result
						}
						ev := ResultEvent{Subtype: raw.Subtype, Error: errMsg, ContextTokens: ctxTokens}
						for _, d := range raw.PermissionDenials {
							if d.ToolName == "" {
								continue
							}
							ev.PermissionDenials = append(ev.PermissionDenials, PermissionDenial(d))
						}
						select {
						case events <- ev:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("claude: scan stream: %w", err)
		}
	}
}
