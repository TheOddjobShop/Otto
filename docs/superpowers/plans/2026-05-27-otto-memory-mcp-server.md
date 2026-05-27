# Otto Memory MCP Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `cmd/otto-memory`, a standalone MCP (Model Context Protocol) stdio server that exposes Plan 1's `internal/memory` and `internal/store` primitives as four tools (`memory_add`, `memory_replace`, `memory_remove`, `session_search`) so Claude Code subprocesses can read/write Otto's persistent memory.

**Architecture:** A thin binary. A `memoryServer` struct holds a `*memory.Core` (the bounded USER.md/MEMORY.md files) and a `*store.Store` (the SQLite turn log). Its handler methods are plain functions — unit-tested directly, with no MCP transport in the loop. `main()` parses `--memory-dir` / `--state-db` flags, constructs the dependencies, registers the four tools via the official Go SDK, and serves over stdio. Domain errors (capacity, duplicate, not-found) are returned as `IsError` tool *results* (so the model reads the message and can self-correct) rather than as transport errors. NO changes to `cmd/otto` in this plan — wiring the server into the bot's `mcp.json` is Plan 3.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk/mcp` v1.6.1, the existing `otto/internal/memory` + `otto/internal/store` packages, stdlib `flag`/`testing`.

---

## Reference: Go SDK v1.6.1 API (verified)

```go
import "github.com/modelcontextprotocol/go-sdk/mcp"

server := mcp.NewServer(&mcp.Implementation{Name: "otto-memory", Version: "v1"}, nil)

type Args struct {
    Field string `json:"field" jsonschema:"description of field"`
}

mcp.AddTool(server, &mcp.Tool{Name: "tool_name", Description: "..."},
    func(ctx context.Context, req *mcp.CallToolRequest, args Args) (*mcp.CallToolResult, any, error) {
        return &mcp.CallToolResult{
            Content: []mcp.Content{&mcp.TextContent{Text: "result"}},
        }, nil, nil
    })

// Error result the model can read (not a transport error):
return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil

server.Run(context.Background(), &mcp.StdioTransport{})  // blocks until stdin closes
```

Handler signature is `func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, Out, error)`. We use `Out = any` and always return `nil` for it.

## File Structure

- `cmd/otto-memory/server.go` — `memoryServer` struct, the four typed arg structs, the four handler methods, `parseTarget`, and a `textResult`/`errResult` helper. All transport-free and unit-testable.
- `cmd/otto-memory/main.go` — flag parsing, dependency construction, tool registration, `server.Run`.
- `cmd/otto-memory/server_test.go` — unit tests calling the handler methods directly against a temp `Core` + `Store`.
- `go.mod` / `go.sum` — add the MCP SDK.

Memory caps are fixed constants in this binary for now (`memCapChars = 2200`, `userCapChars = 1375`); Plan 3 can promote them to config. Search limit default is `8`.

---

## Task 1: Add the MCP Go SDK dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add the dependency**

Run:
```bash
go get github.com/modelcontextprotocol/go-sdk@v1.6.1
```
Expected: `go.mod` gains `require github.com/modelcontextprotocol/go-sdk v1.6.1`; `go.sum` updated.

- [ ] **Step 2: Verify the module still builds**

Run: `go build ./...`
Expected: exit 0.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "build: add modelcontextprotocol/go-sdk for otto-memory MCP server"
```

---

## Task 2: memoryServer + memory_add/replace/remove handlers

**Files:**
- Create: `cmd/otto-memory/server.go`
- Test: `cmd/otto-memory/server_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/otto-memory/server_test.go`:
```go
package main

import (
	"context"
	"strings"
	"testing"

	"otto/internal/memory"
	"otto/internal/store"
)

func newTestServer(t *testing.T) *memoryServer {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(dir + "/state.db")
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return &memoryServer{
		core:  memory.NewCore(dir, 2200, 1375),
		store: st,
	}
}

func TestHandleAddThenItAppearsInFiles(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()

	res, _, err := s.handleAdd(ctx, nil, addArgs{Target: "user", Content: "User is named Justin."})
	if err != nil {
		t.Fatalf("handleAdd returned transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleAdd reported tool error: %s", resultText(res))
	}
	user, _, _ := s.core.Load()
	if !strings.Contains(user, "Justin") {
		t.Fatalf("added content not persisted: %q", user)
	}
}

func TestHandleAddRejectsBadTarget(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleAdd(context.Background(), nil, addArgs{Target: "bogus", Content: "x"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("expected IsError result for bad target")
	}
}

func TestHandleAddSurfacesDomainErrorAsIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleAdd(context.Background(), nil, addArgs{Target: "user", Content: "sk-ant-api03-shouldBeRejected"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("secret content should produce an IsError tool result")
	}
}

func TestHandleReplaceAndRemove(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if _, _, err := s.handleAdd(ctx, nil, addArgs{Target: "memory", Content: "Server runs Ubuntu."}); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	res, _, err := s.handleReplace(ctx, nil, replaceArgs{Target: "memory", OldText: "Ubuntu", Content: "Arch Linux"})
	if err != nil || res.IsError {
		t.Fatalf("handleReplace failed: err=%v res=%q", err, resultText(res))
	}
	_, mem, _ := s.core.Load()
	if !strings.Contains(mem, "Arch Linux") || strings.Contains(mem, "Ubuntu") {
		t.Fatalf("replace not applied: %q", mem)
	}
	res, _, err = s.handleRemove(ctx, nil, removeArgs{Target: "memory", OldText: "Server runs Arch Linux."})
	if err != nil || res.IsError {
		t.Fatalf("handleRemove failed: err=%v res=%q", err, resultText(res))
	}
	_, mem, _ = s.core.Load()
	if strings.Contains(mem, "Arch Linux") {
		t.Fatalf("entry not removed: %q", mem)
	}
}

func TestHandleRemoveMissingIsError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleRemove(context.Background(), nil, removeArgs{Target: "memory", OldText: "not there"})
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !res.IsError {
		t.Fatal("removing missing text should be an IsError result")
	}
}

// resultText extracts the concatenated text of a tool result for assertions.
func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
```

Note: `resultText` references `mcp` — add `"github.com/modelcontextprotocol/go-sdk/mcp"` to the test's import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto-memory/ -run TestHandle -v`
Expected: FAIL — `undefined: memoryServer` / `undefined: addArgs` etc.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/otto-memory/server.go`:
```go
// Command otto-memory is an MCP stdio server exposing Otto's persistent
// memory: the bounded curated core (USER.md/MEMORY.md) and FTS5 keyword
// search over the conversation turn log. It is launched by Claude Code via
// Otto's mcp.json (wired in a later plan); it is not part of the otto binary.
package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/memory"
	"otto/internal/store"
)

// memoryServer holds the dependencies the MCP tool handlers operate on.
type memoryServer struct {
	core  *memory.Core
	store *store.Store
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto-memory/ -run TestHandle -v`
Expected: PASS (all five subtests).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto-memory/server.go cmd/otto-memory/server_test.go
git commit -m "feat(otto-memory): memory_add/replace/remove MCP handlers"
```

---

## Task 3: session_search handler

**Files:**
- Modify: `cmd/otto-memory/server.go`
- Modify: `cmd/otto-memory/server_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto-memory/server_test.go`:
```go
func TestHandleSearchFindsTurns(t *testing.T) {
	s := newTestServer(t)
	ctx := context.Background()
	if _, err := s.store.AppendTurn(ctx, "otto", "user", "remind me about the Tokyo trip"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	if _, err := s.store.AppendTurn(ctx, "otto", "assistant", "your Tokyo flight is at 9am"); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "Tokyo"})
	if err != nil {
		t.Fatalf("handleSearch transport error: %v", err)
	}
	if res.IsError {
		t.Fatalf("handleSearch reported error: %s", resultText(res))
	}
	text := resultText(res)
	if !strings.Contains(text, "Tokyo") {
		t.Fatalf("search result should mention the matched content: %q", text)
	}
}

func TestHandleSearchNoMatchesIsNotError(t *testing.T) {
	s := newTestServer(t)
	res, _, err := s.handleSearch(context.Background(), nil, searchArgs{Query: "nonexistent"})
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if res.IsError {
		t.Fatal("a no-match search is a normal empty result, not an error")
	}
	if !strings.Contains(strings.ToLower(resultText(res)), "no") {
		t.Fatalf("empty search should say so, got: %q", resultText(res))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto-memory/ -run TestHandleSearch -v`
Expected: FAIL — `undefined: (*memoryServer).handleSearch` / `undefined: searchArgs`.

- [ ] **Step 3: Write minimal implementation**

Append to `cmd/otto-memory/server.go`:
```go
// defaultSearchLimit bounds how many turns session_search returns when the
// caller does not specify a limit.
const defaultSearchLimit = 8

type searchArgs struct {
	Query string `json:"query" jsonschema:"keywords to look for in past conversation turns"`
	Limit int    `json:"limit,omitempty" jsonschema:"max results (default 8)"`
}

func (s *memoryServer) handleSearch(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	turns, err := s.store.SearchFTS(ctx, args.Query, limit)
	if err != nil {
		return errResult(fmt.Sprintf("search failed: %v", err)), nil, nil
	}
	if len(turns) == 0 {
		return textResult("No matching past conversation turns."), nil, nil
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%d matching turn(s):\n", len(turns)))
	for _, tr := range turns {
		b.WriteString(fmt.Sprintf("\n[%s/%s @ %s] %s",
			tr.Persona, tr.Role, tr.TS.Format("2006-01-02 15:04"), tr.Content))
	}
	return textResult(b.String()), nil, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto-memory/ -v`
Expected: PASS (whole package — all Handle* tests).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto-memory/server.go cmd/otto-memory/server_test.go
git commit -m "feat(otto-memory): session_search MCP handler over FTS5 turn log"
```

---

## Task 4: main — flags, wiring, stdio serve

**Files:**
- Create: `cmd/otto-memory/main.go`

- [ ] **Step 1: Write the implementation**

There is no unit test for `main` (it blocks on stdio); correctness is covered by the handler tests in Tasks 2–3 plus the build + smoke check in Steps 2–3. Create `cmd/otto-memory/main.go`:
```go
package main

import (
	"context"
	"flag"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"otto/internal/memory"
	"otto/internal/store"
)

// Memory core character caps (rough token proxies). Promote to config in a
// later plan if they need to be tunable per deployment.
const (
	memCapChars  = 2200 // MEMORY.md ~800 tokens
	userCapChars = 1375 // USER.md   ~500 tokens
)

func main() {
	memDir := flag.String("memory-dir", "", "directory holding USER.md and MEMORY.md (required)")
	stateDB := flag.String("state-db", "", "path to the SQLite turn-log database (required)")
	flag.Parse()

	if *memDir == "" || *stateDB == "" {
		log.Fatal("otto-memory: --memory-dir and --state-db are required")
	}

	st, err := store.Open(*stateDB)
	if err != nil {
		log.Fatalf("otto-memory: open store: %v", err)
	}
	defer st.Close()

	srv := &memoryServer{
		core:  memory.NewCore(*memDir, memCapChars, userCapChars),
		store: st,
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "otto-memory", Version: "v1"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_add",
		Description: "Save a durable fact to long-term memory. Use for corrections, discovered preferences, environment facts, project conventions, and lessons — not ephemera. target is \"user\" (about the person) or \"memory\" (everything else).",
	}, srv.handleAdd)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_replace",
		Description: "Replace a unique existing memory entry with updated text. Used to consolidate or correct facts. Matching is raw substring; pass a distinctive snippet.",
	}, srv.handleReplace)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "memory_remove",
		Description: "Delete a unique existing memory entry. Matching is raw substring; pass a distinctive snippet.",
	}, srv.handleRemove)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "session_search",
		Description: "Keyword-search past conversation turns (\"what did we discuss about X\"). Returns the most relevant matching turns.",
	}, srv.handleSearch)

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Printf("otto-memory: server exited: %v", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Build the binary**

Run: `go build ./cmd/otto-memory/`
Expected: exit 0, produces an `otto-memory` binary (delete it after: `rm -f otto-memory`).

- [ ] **Step 3: Smoke-test the MCP handshake over stdio**

An MCP server responds to a JSON-RPC `initialize` then `tools/list`. Verify the server starts, lists exactly our four tools, and exits cleanly when stdin closes:
```bash
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  | go run ./cmd/otto-memory/ --memory-dir "$(mktemp -d)" --state-db "$(mktemp -d)/state.db"
```
Expected: two JSON-RPC response lines on stdout. The second response's `result.tools` array contains four entries with names `memory_add`, `memory_replace`, `memory_remove`, `session_search`. The process exits 0 after stdin EOF. (If the protocolVersion string is rejected by this SDK build, the error will name the version it expects — use that; the tool list is what matters.)

- [ ] **Step 4: Commit**

```bash
git add cmd/otto-memory/main.go
git commit -m "feat(otto-memory): main — flags, tool registration, stdio serve"
```

---

## Task 5: Final verification

- [ ] **Step 1: Vet + format**

Run:
```bash
go vet ./cmd/otto-memory/
gofmt -l cmd/otto-memory/
```
Expected: vet exits 0; gofmt prints nothing.

- [ ] **Step 2: Full module build + test (+ race on the new package)**

Run:
```bash
go build ./...
go test ./...
go test -race ./cmd/otto-memory/
```
Expected: all exit 0. Pre-existing packages unaffected; `cmd/otto-memory` tests pass with no races.

- [ ] **Step 3: Confirm cmd/otto is untouched**

Run: `git diff master --stat -- cmd/otto/`
Expected: no output (this plan adds `cmd/otto-memory/` only; wiring into the bot is Plan 3).

---

## Self-Review notes

- **Spec coverage (this plan's slice):** `otto-memory` MCP server exposing `memory_add`/`memory_replace`/`memory_remove` → Task 2; `session_search` (FTS5 keyword) → Task 3; security scan + capacity + dedup are inherited from `memory.Core` and surfaced as readable `IsError` results so the model can consolidate → Tasks 2–3. Deferred (correctly out of scope): embedder chain / semantic merge into search (Plan 4), injector + turn-logging + mcp.json registration + config/setup (Plan 3), rotator (Plan 4).
- **Type consistency:** handler methods `handleAdd`/`handleReplace`/`handleRemove`/`handleSearch`; arg structs `addArgs`/`replaceArgs`/`removeArgs`/`searchArgs`; helpers `parseTarget`/`textResult`/`errResult`; deps `memoryServer{core *memory.Core, store *store.Store}`. All defined in Task 2/3 and reused consistently. Uses Plan 1 APIs exactly: `store.Open`, `Store.AppendTurn`, `Store.SearchFTS`, `Turn{Persona,Role,Content,TS}`, `memory.NewCore(dir, memCap, userCap)`, `Core.Add/Replace/Remove/Load`, `memory.TargetUser/TargetMemory`.
- **Note for Plan 3:** the bot must launch this binary from `mcp.json` with `--memory-dir <memory_dir>` and `--state-db <state_db_path>` pointing at the SAME paths Otto's own `memory.Core`/`store.Store` use, so the injected core and the tool-written core are one and the same.
- **SDK risk:** the exact `protocolVersion` accepted by go-sdk v1.6.1 may differ from the smoke-test string; Task 4 Step 3 notes this and the tool-list assertion is the real check. If `mcp.AddTool`'s generic inference rejects a handler method value, wrap it in an explicit `func(...) (...) {...}` literal — behavior is identical.
```
