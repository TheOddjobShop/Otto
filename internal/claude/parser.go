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
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Message struct {
		Content []rawContentBlock `json:"content"`
	} `json:"message"`
}

// rawContentBlock covers every content-block shape Otto reads, across both
// `assistant` frames (text, tool_use) and `user` frames (tool_result).
// Unknown block types decode into zero values and are skipped by the switch in
// ParseStream, so a new block type upstream is inert rather than fatal.
type rawContentBlock struct {
	Type string `json:"type"`

	// text blocks
	Text string `json:"text"`

	// tool_use blocks
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`

	// tool_result blocks. Content is either a bare string or an array of
	// content blocks depending on the tool, so it stays raw until
	// flattenToolResult decides which.
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// toolResultTextCap bounds how much tool-result text the parser carries into a
// ToolResultEvent. Results can be megabytes (a file read, a verbose test run);
// the event feeds a one-line activity entry, so anything past this is waste
// that would otherwise be copied through the channel and into SQLite.
const toolResultTextCap = 2000

// flattenToolResult extracts displayable text from a tool_result content field,
// which the API types as `string | ContentBlock[]`. Returns "" when neither
// shape yields text (e.g. an image-only result).
func flattenToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Shape 1: a bare JSON string.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return truncateRunes(s, toolResultTextCap)
	}
	// Shape 2: an array of blocks; concatenate the text ones.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b bytes.Buffer
	for _, blk := range blocks {
		if blk.Type == "text" && blk.Text != "" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(blk.Text)
			if b.Len() >= toolResultTextCap {
				break
			}
		}
	}
	return truncateRunes(b.String(), toolResultTextCap)
}

// truncateRunes caps s at n runes, never splitting a multi-byte sequence.
func truncateRunes(s string, n int) string {
	if len(s) <= n { // byte length >= rune count, so this is a safe fast path
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
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
							var ev Event
							switch {
							case c.Type == "text" && c.Text != "":
								ev = AssistantTextEvent{Text: c.Text}
							case c.Type == "tool_use" && c.Name != "":
								ev = ToolUseEvent{ID: c.ID, Name: c.Name, Input: c.Input}
							default:
								continue
							}
							select {
							case events <- ev:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
					case "user":
						// Tool results come back as user-role frames. Otto sends
						// no user frames of its own mid-stream, so anything here
						// is Claude Code reporting a tool outcome.
						for _, c := range raw.Message.Content {
							if c.Type != "tool_result" {
								continue
							}
							select {
							case events <- ToolResultEvent{
								ToolUseID: c.ToolUseID,
								IsError:   c.IsError,
								Content:   flattenToolResult(c.Content),
							}:
							case <-ctx.Done():
								return ctx.Err()
							}
						}
					case "result":
						ctxTokens := raw.Usage.InputTokens + raw.Usage.CacheCreationInputTokens + raw.Usage.CacheReadInputTokens
						errMsg := raw.Error
						if errMsg == "" && raw.Subtype != "success" {
							errMsg = raw.Result
						}
						ev := ResultEvent{
							Subtype:             raw.Subtype,
							Error:               errMsg,
							ContextTokens:       ctxTokens,
							InputTokens:         raw.Usage.InputTokens,
							OutputTokens:        raw.Usage.OutputTokens,
							CacheCreationTokens: raw.Usage.CacheCreationInputTokens,
							CacheReadTokens:     raw.Usage.CacheReadInputTokens,
						}
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
