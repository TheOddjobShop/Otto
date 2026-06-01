// Command otto-memory is an MCP stdio server exposing Otto's persistent
// memory: the bounded curated core (USER.md/MEMORY.md) and FTS5 keyword
// search over the conversation turn log. It is launched by Claude Code via
// Otto's mcp.json (wired in a later plan); it is not part of the otto binary.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
)

// memoryServer holds the dependencies the MCP tool handlers operate on.
type memoryServer struct {
	core     *memory.Core
	store    *store.Store
	embedder embed.Embedder // optional; nil = keyword-only search
}

type addArgs struct {
	Target  string `json:"target" jsonschema:"which file to write: \"user\" (identity/preferences) or \"memory\" (environment/projects/lessons)"`
	Content string `json:"content" jsonschema:"a single dense, declarative fact to remember"`
}

type replaceArgs struct {
	Target  string `json:"target" jsonschema:"\"user\" or \"memory\""`
	OldText string `json:"old_text" jsonschema:"a distinctive snippet of the existing entry to replace (raw substring, must be unique)"`
	Content string `json:"content" jsonschema:"the new text to put in its place"`
}

type removeArgs struct {
	Target  string `json:"target" jsonschema:"\"user\" or \"memory\""`
	OldText string `json:"old_text" jsonschema:"a distinctive snippet of the entry to delete (raw substring, must be unique)"`
}

// parseTarget maps the tool's string target to a memory.Target.
func parseTarget(s string) (memory.Target, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "user":
		return memory.TargetUser, nil
	case "memory":
		return memory.TargetMemory, nil
	}
	return 0, fmt.Errorf("invalid target %q: use \"user\" or \"memory\"", s)
}

// textResult wraps a plain success message.
func textResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

// errResult wraps a message as a tool error the model can read and act on
// (e.g. a capacity message telling it to consolidate). It is NOT a transport
// error — the handler still returns nil for the Go error.
func errResult(msg string) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}
}

func (s *memoryServer) handleAdd(ctx context.Context, req *mcp.CallToolRequest, args addArgs) (*mcp.CallToolResult, any, error) {
	t, err := parseTarget(args.Target)
	if err != nil {
		return errResult(err.Error()), nil, nil
	}
	if err := s.core.Add(t, args.Content); err != nil {
		return errResult(err.Error()), nil, nil
	}
	return textResult("Stored."), nil, nil
}

func (s *memoryServer) handleReplace(ctx context.Context, req *mcp.CallToolRequest, args replaceArgs) (*mcp.CallToolResult, any, error) {
	t, err := parseTarget(args.Target)
	if err != nil {
		return errResult(err.Error()), nil, nil
	}
	if err := s.core.Replace(t, args.OldText, args.Content); err != nil {
		return errResult(err.Error()), nil, nil
	}
	return textResult("Replaced."), nil, nil
}

func (s *memoryServer) handleRemove(ctx context.Context, req *mcp.CallToolRequest, args removeArgs) (*mcp.CallToolResult, any, error) {
	t, err := parseTarget(args.Target)
	if err != nil {
		return errResult(err.Error()), nil, nil
	}
	if err := s.core.Remove(t, args.OldText); err != nil {
		return errResult(err.Error()), nil, nil
	}
	return textResult("Removed."), nil, nil
}

// defaultSearchLimit bounds how many turns session_search returns when the
// caller does not specify a limit.
const defaultSearchLimit = 8

// queryEmbedTimeout caps how long session_search waits on the embedder before
// falling back to keyword-only search. Short so a cold/missing Ollama model
// can't stall the tool call the model is waiting on.
const queryEmbedTimeout = 6 * time.Second

// maxTurnContentChars bounds how much of each matched turn's content
// session_search echoes back, so one very long stored turn can't blow up the
// tool response (which is fed straight into the model's context).
const maxTurnContentChars = 280

// truncateContent shortens s to maxTurnContentChars runes, appending an
// ellipsis when truncated. Rune-based so it never splits a multi-byte char.
func truncateContent(s string) string {
	r := []rune(s)
	if len(r) <= maxTurnContentChars {
		return s
	}
	return string(r[:maxTurnContentChars]) + "…"
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"keywords to look for in past conversation turns"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results (default 8)"`
}

func (s *memoryServer) handleSearch(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	var semantic []store.Turn
	if s.embedder != nil {
		ectx, ecancel := context.WithTimeout(ctx, queryEmbedTimeout)
		r, err := s.embedder.Embed(ectx, args.Query)
		ecancel()
		if err == nil {
			if sem, serr := s.store.SearchSemantic(ctx, r.Vector, limit); serr == nil {
				semantic = sem
			} else {
				log.Printf("session_search: semantic: %v", serr)
			}
		} else {
			log.Printf("session_search: embed unavailable, keyword-only: %v", err)
		}
	}

	fts, ferr := s.store.SearchFTS(ctx, args.Query, limit)
	if ferr != nil && len(semantic) == 0 {
		return errResult(fmt.Sprintf("search failed: %v", ferr)), nil, nil
	}

	turns := mergeTurns(semantic, fts, limit)
	if len(turns) == 0 {
		return textResult("No matching past conversation turns."), nil, nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d matching turn(s):\n", len(turns)))
	for _, tr := range turns {
		b.WriteString(fmt.Sprintf("\n[%s/%s @ %s] %s",
			tr.Persona, tr.Role, tr.TS.Format("2006-01-02 15:04"), truncateContent(tr.Content)))
	}
	return textResult(b.String()), nil, nil
}

// forwardArgs is the schema for the forward_to_otto tool. Both fields are
// required; the reason is shown to the user verbatim in the bus banner so
// they know why Toto handed the message off.
type forwardArgs struct {
	Message string `json:"message" jsonschema:"the user's request, in their voice (cleaned up if rambly), to hand off to Otto"`
	Reason  string `json:"reason" jsonschema:"a short one-line reason — e.g. \"user wants gmail summary\" — shown in the visible banner"`
}

// handleForward queues a user-meant message for Otto via the inbox bus.
// Toto calls this when the user's message is actually work for Otto
// (running code, sending email, anything Otto handles). The body is
// formatted with a small "(from toto — <reason>)" prefix so Otto reads
// the message with context about who handed it off and why.
//
// Refuses with a model-readable message when the agent-hop guard fires,
// so the model knows not to retry inside a nested dispatch.
func (s *memoryServer) handleForward(ctx context.Context, req *mcp.CallToolRequest, args forwardArgs) (*mcp.CallToolResult, any, error) {
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return errResult("forward_to_otto refused: message is empty"), nil, nil
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return errResult("forward_to_otto refused: reason is empty (the user needs to see why you handed off)"), nil, nil
	}
	body := "(from toto — " + reason + ")\n\n" + msg
	if _, err := s.store.Enqueue(ctx, "otto", "agent", "toto", body); err != nil {
		if errors.Is(err, store.ErrBusLoopGuard) {
			return errResult("forward_to_otto refused: nested agent forwards not allowed"), nil, nil
		}
		return errResult(fmt.Sprintf("forward_to_otto failed: %v", err)), nil, nil
	}
	return textResult("Queued for Otto."), nil, nil
}

// messageArgs is the schema for the message_toto / message_toot tools. Both
// fields are required; the reason is shown to the user verbatim in the bus
// banner so they know why Otto pinged a pet.
type messageArgs struct {
	Message string `json:"message" jsonschema:"the message Otto wants the pet to deliver, in Otto's voice"`
	Reason  string `json:"reason" jsonschema:"a short one-line reason — e.g. \"finishing report\" — shown in the visible banner"`
}

// handleMessageToto queues an Otto-authored message addressed to Toto via the
// inbox bus. Mirrors handleForward but in the opposite direction: Otto picks
// Toto when chitchat fits or just for the love of the game. The body is
// prefixed with "(from otto — <reason>)" so Toto reads the message with
// context about why Otto pinged.
//
// Refuses with a model-readable message when the agent-hop guard fires,
// so the model knows nested agent forwards aren't allowed.
func (s *memoryServer) handleMessageToto(ctx context.Context, req *mcp.CallToolRequest, args messageArgs) (*mcp.CallToolResult, any, error) {
	return s.enqueueFromOtto(ctx, "toto", "message_toto", "Sent to Toto.", args)
}

// handleMessageToot queues an Otto-authored message addressed to Toot via the
// inbox bus. Same shape as message_toto; Otto picks Toot when something is
// list-shaped or release-shaped and the clipboard-owl voice fits.
func (s *memoryServer) handleMessageToot(ctx context.Context, req *mcp.CallToolRequest, args messageArgs) (*mcp.CallToolResult, any, error) {
	return s.enqueueFromOtto(ctx, "toot", "message_toot", "Sent to Toot.", args)
}

// enqueueFromOtto is the shared body of handleMessageToto / handleMessageToot.
// The tool name is woven into every diagnostic so the model can tell which
// call refused without parsing free-form text.
func (s *memoryServer) enqueueFromOtto(ctx context.Context, target, tool, ok string, args messageArgs) (*mcp.CallToolResult, any, error) {
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return errResult(tool + " refused: message is empty"), nil, nil
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return errResult(tool + " refused: reason is empty (the user needs to see why you pinged)"), nil, nil
	}
	body := "(from otto — " + reason + ")\n\n" + msg
	if _, err := s.store.Enqueue(ctx, target, "agent", "otto", body); err != nil {
		if errors.Is(err, store.ErrBusLoopGuard) {
			return errResult(tool + " refused: nested agent forwards not allowed"), nil, nil
		}
		return errResult(fmt.Sprintf("%s failed: %v", tool, err)), nil, nil
	}
	return textResult(ok), nil, nil
}

// mergeTurns combines semantic and keyword results, semantic first, deduped by
// turn ID, capped at limit. Semantic hits rank by meaning; keyword hits fill
// the remainder (catching exact tokens vectors miss).
func mergeTurns(semantic, fts []store.Turn, limit int) []store.Turn {
	seen := make(map[int64]bool)
	out := make([]store.Turn, 0, limit)
	for _, group := range [][]store.Turn{semantic, fts} {
		for _, t := range group {
			if len(out) >= limit {
				return out
			}
			if seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	return out
}
