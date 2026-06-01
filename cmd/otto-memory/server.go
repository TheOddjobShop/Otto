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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
)

// envBusHop is the environment variable the cmd/otto bus dispatcher sets
// on each Claude subprocess (which in turn passes env through to its MCP
// stdio children, including this one). It carries the bus-hop counter of
// the message currently being processed by the recipient agent; absent
// means the agent isn't running on behalf of a bus dispatch (e.g. it's
// handling a fresh user message) and outgoing forwards start at hop 1.
const envBusHop = "OTTO_BUS_HOP"

// envBusSender is the environment variable that names the agent currently
// running. Used to stamp the "(from <name> — …)" preamble on enqueued
// bodies so the receiver can tell who pinged them without parsing args.
const envBusSender = "OTTO_BUS_SENDER"

// hopFromCtxOrEnv returns the effective bus hop for the current request:
// the ctx value (set in-process by tests / cmd/otto's own dispatch paths)
// wins; otherwise the env var the dispatcher attached to the parent
// Claude process is read. Returns 0 when neither is present (the legitimate
// "agent runs on a plain user prompt" case).
func hopFromCtxOrEnv(ctx context.Context) int {
	if n, ok := store.BusHopFromCtx(ctx); ok {
		return n
	}
	if v := os.Getenv(envBusHop); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// senderFromCtxOrEnv returns the agent name to stamp on outgoing bus
// rows. Mirrors hopFromCtxOrEnv: ctx for tests / in-process paths, env
// for production cross-process.
func senderFromCtxOrEnv(ctx context.Context, fallback string) string {
	if s, ok := senderFromCtx(ctx); ok && s != "" {
		return s
	}
	if v := strings.TrimSpace(os.Getenv(envBusSender)); v != "" {
		return v
	}
	return fallback
}

// ctxKeyBusSender carries the running agent's name through in-process
// dispatch paths (mirror of the env-var transport used in production).
type ctxKeyBusSender struct{}

// WithBusSender returns a child context labelled with the agent name
// running the current turn. The MCP tool handlers consult it to stamp
// outgoing bus rows accurately. In production the same information rides
// via the OTTO_BUS_SENDER env var on the Claude subprocess.
func WithBusSender(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, ctxKeyBusSender{}, name)
}

// senderFromCtx reads the WithBusSender value.
func senderFromCtx(ctx context.Context) (string, bool) {
	s, ok := ctx.Value(ctxKeyBusSender{}).(string)
	return s, ok
}

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
// Refuses with a model-readable message when the hop cap is reached, so
// the model knows the chain is over rather than retrying.
func (s *memoryServer) handleForward(ctx context.Context, req *mcp.CallToolRequest, args forwardArgs) (*mcp.CallToolResult, any, error) {
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return errResult("forward_to_otto refused: message is empty"), nil, nil
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return errResult("forward_to_otto refused: reason is empty (the user needs to see why you handed off)"), nil, nil
	}
	sender := senderFromCtxOrEnv(ctx, "toto")
	body := "(from " + sender + " — " + reason + ")\n\n" + msg
	hop := hopFromCtxOrEnv(ctx)
	if _, err := s.store.Enqueue(ctx, "otto", "agent", sender, body, hop+1); err != nil {
		if errors.Is(err, store.ErrBusHopExceeded) {
			return errResult(fmt.Sprintf("forward_to_otto refused: agent-to-agent conversation reached its %d-hop cap; ending here.", store.MaxBusHop)), nil, nil
		}
		return errResult(fmt.Sprintf("forward_to_otto failed: %v", err)), nil, nil
	}
	return textResult("Queued for Otto."), nil, nil
}

// messageArgs is the schema for the message_toto / message_toot tools. Both
// fields are required; the reason is shown to the user verbatim in the bus
// banner so they know why an agent pinged a pet.
type messageArgs struct {
	Message string `json:"message" jsonschema:"the message to deliver, in the caller's voice"`
	Reason  string `json:"reason" jsonschema:"a short one-line reason — e.g. \"finishing report\" — shown in the visible banner"`
}

// handleMessageToto queues an agent-authored message addressed to Toto via
// the inbox bus. The caller is read from the WithBusHop ctx (set by the
// dispatcher) so the same handler serves Otto-from-default and Toot-from-
// bus dispatches. The body is prefixed with "(from <sender> — <reason>)"
// so Toto reads the message with context about why he was pinged.
//
// Refuses with a model-readable message when the hop cap is reached so
// the model knows to stop the chain.
func (s *memoryServer) handleMessageToto(ctx context.Context, req *mcp.CallToolRequest, args messageArgs) (*mcp.CallToolResult, any, error) {
	return s.enqueueAgentMessage(ctx, "toto", "message_toto", "Sent to Toto.", args)
}

// handleMessageToot queues an agent-authored message addressed to Toot via
// the inbox bus. Same shape as message_toto; the dispatcher's hop counter
// drives the cap.
func (s *memoryServer) handleMessageToot(ctx context.Context, req *mcp.CallToolRequest, args messageArgs) (*mcp.CallToolResult, any, error) {
	return s.enqueueAgentMessage(ctx, "toot", "message_toot", "Sent to Toot.", args)
}

// enqueueAgentMessage is the shared body of handleMessageToto /
// handleMessageToot. The tool name is woven into every diagnostic so the
// model can tell which call refused without parsing free-form text. The
// sender is inferred from the recipient: if the bus-ctx has no hop set,
// this is Otto's initial outbound; otherwise the chain alternates so the
// sender is whichever non-target agent fits the addressing.
func (s *memoryServer) enqueueAgentMessage(ctx context.Context, target, tool, ok string, args messageArgs) (*mcp.CallToolResult, any, error) {
	msg := strings.TrimSpace(args.Message)
	if msg == "" {
		return errResult(tool + " refused: message is empty"), nil, nil
	}
	reason := strings.TrimSpace(args.Reason)
	if reason == "" {
		return errResult(tool + " refused: reason is empty (the user needs to see why you pinged)"), nil, nil
	}
	sender := senderFromCtxOrEnv(ctx, defaultSenderFor(target))
	body := "(from " + sender + " — " + reason + ")\n\n" + msg
	hop := hopFromCtxOrEnv(ctx)
	if _, err := s.store.Enqueue(ctx, target, "agent", sender, body, hop+1); err != nil {
		if errors.Is(err, store.ErrBusHopExceeded) {
			return errResult(fmt.Sprintf("%s refused: agent-to-agent conversation reached its %d-hop cap; ending here.", tool, store.MaxBusHop)), nil, nil
		}
		return errResult(fmt.Sprintf("%s failed: %v", tool, err)), nil, nil
	}
	return textResult(ok), nil, nil
}

// defaultSenderFor returns the agent expected to be the most likely sender
// when neither ctx nor env var names one. message_toto without context is
// Otto pinging Toto; message_toot likewise. forward_to_otto is Toto handing
// off to Otto.
func defaultSenderFor(target string) string {
	if target == "otto" {
		return "toto"
	}
	return "otto"
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
