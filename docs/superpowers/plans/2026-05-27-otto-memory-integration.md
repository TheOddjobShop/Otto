# Otto Memory Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Wire the Plan 1/2 memory machinery into the live Otto bot: inject the curated memory core (USER.md/MEMORY.md) into every Otto/Toto/Toot prompt, log every conversation turn to `state.db`, construct the store + core in `main`, and register the `otto-memory` MCP server in `mcp.json` via `setup.sh` so Claude can call the memory tools.

**Architecture:** Two small new helpers in `cmd/otto/memory.go` — `composeMemoryPrompt(base, *memory.Core)` (append the injected core to a system prompt) and `logTurn(ctx, *store.Store, persona, role, content)` (best-effort turn logging). The handler, Toto, and Toot each gain `mem *memory.Core` + `store *store.Store` fields (and the handler a `baseSystemPrompt string`); all three compose memory into their per-call `AppendSystemPrompt` and log their turns. `main` opens the store (creating its parent dir), builds the core from new config paths, and wires the fields. `config` gains optional `memory_dir` / `state_db_path` with defaults derived from `session_id_path`. `setup.sh` builds the `otto-memory` binary and adds it to `mcp.json` with `--memory-dir`/`--state-db` pointing at the same paths Otto uses. Everything degrades gracefully when `mem`/`store` are nil (existing tests untouched).

**Tech Stack:** Go 1.26, existing `otto/internal/memory` + `otto/internal/store`, `cmd/otto` (build tag `//go:build unix`), `setup.sh` (bash + inline python3), `internal/config` (BurntSushi/toml).

---

## Key facts about the existing code (so edits land correctly)

- `cmd/otto` files carry `//go:build unix` as the first line. New files in this package MUST too.
- `handler` struct (`cmd/otto/handler.go`) fields today: `bot, allow, session, runner, startedAt, otto, toto, updater, pets, dispatchWG`.
- `handler.handleMessage` builds `claude.RunArgs{Prompt, SessionID, ImagePaths}` and calls `runAndReply(callCtx, ctx, chatID, args)`. `runAndReply` sends the assistant text via `telegram.SendChunked(sendCtx, h.bot, chatID, out)` near its end.
- `claude.RunArgs.AppendSystemPrompt` (string): when non-empty it REPLACES the runner's configured system prompt for that one call (see `internal/claude/runner.go`). Otto's runner is built with the base prompt; setting `AppendSystemPrompt` to `base + memory` per call is how we inject live memory.
- `Toto.replyWithContext` (`cmd/otto/toto.go`) builds a local `systemPrompt` string then calls `t.runner.Run(ctx, claude.RunArgs{..., AppendSystemPrompt: systemPrompt, ...})`, and sends via `t.send(ctx, chatID, out)`.
- `Toot.Reply` (`cmd/otto/toot.go`) builds `systemPrompt`, runs, then `t.deliver(ctx, chatID, out)`.
- `internal/store.Store`: `Open(path)`, `Close()`, `AppendTurn(ctx, persona, role, content) (int64, error)`, `SearchFTS(ctx, query, limit)`. `Open` creates the DB file but NOT its parent directory.
- `internal/memory.Core`: `NewCore(dir, memCap, userCap)`, `Inject() (string, error)` (returns "" when empty), `Load()`.
- Test scaffolding (`cmd/otto/handler_test.go`): `fakeRunner` records each `RunArgs` in `.called`; `fakeBot` records sent messages in `.sent`; `newTestHandler(t, bot, runner)` builds a handler with `toto` wired but `mem`/`store` nil.
- `setup.sh` vars: `OTTO_STATE_DIR="$HOME/.local/state/otto"`, `OTTO_BIN` (the otto binary path under `~/.local/bin`), `MCP_FILE`, `CONFIG_FILE`. `write_toml_field <key> <value> <file>` writes a TOML key. `mcp.json` is produced by an inline `python3` heredoc that builds a `config["mcpServers"]` dict.

## File Structure

- `internal/config/config.go` (modify) — add `MemoryDir`, `StateDBPath` fields + default derivation.
- `internal/config/config_test.go` (modify) — tests for defaults + explicit values.
- `cmd/otto/memory.go` (create) — `composeMemoryPrompt`, `logTurn`, the `memCapChars`/`userCapChars` consts.
- `cmd/otto/memory_test.go` (create) — unit tests for both helpers.
- `cmd/otto/handler.go` (modify) — add `mem`, `store`, `baseSystemPrompt` fields; inject + log in Otto's path.
- `cmd/otto/toto.go` (modify) — add `mem`, `store`; inject + log.
- `cmd/otto/toot.go` (modify) — add `mem`, `store`; inject + log.
- `cmd/otto/handler_test.go` (modify) — extend a test to assert Otto injection + logging.
- `cmd/otto/main.go` (modify) — open store, build core, wire fields, close store on shutdown.
- `setup.sh` (modify) — build `otto-memory`, mkdir memory dir, write config fields, add server to `mcp.json`.

---

## Task 1: config — memory_dir + state_db_path with defaults

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go` (if the file lacks helpers to write a temp config, this test writes its own TOML):
```go
func TestLoadDerivesMemoryDefaultsFromSessionPath(t *testing.T) {
	dir := t.TempDir()
	// Minimal valid config. claude_binary_path and mcp_config_path must exist
	// on disk for validation to pass, so point them at real temp files.
	bin := filepath.Join(dir, "claude")
	mcp := filepath.Join(dir, "mcp.json")
	for _, p := range []string{bin, mcp} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	body := "telegram_bot_token = \"t\"\n" +
		"telegram_allowed_user_id = 5\n" +
		"claude_binary_path = \"" + bin + "\"\n" +
		"mcp_config_path = \"" + mcp + "\"\n" +
		"session_id_path = \"" + dir + "/session_id\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantMem := filepath.Join(dir, "memory")
	wantDB := filepath.Join(dir, "state.db")
	if cfg.MemoryDir != wantMem {
		t.Errorf("MemoryDir = %q, want %q", cfg.MemoryDir, wantMem)
	}
	if cfg.StateDBPath != wantDB {
		t.Errorf("StateDBPath = %q, want %q", cfg.StateDBPath, wantDB)
	}
}

func TestLoadHonorsExplicitMemoryPaths(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	mcp := filepath.Join(dir, "mcp.json")
	for _, p := range []string{bin, mcp} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	body := "telegram_bot_token = \"t\"\n" +
		"telegram_allowed_user_id = 5\n" +
		"claude_binary_path = \"" + bin + "\"\n" +
		"mcp_config_path = \"" + mcp + "\"\n" +
		"session_id_path = \"" + dir + "/session_id\"\n" +
		"memory_dir = \"/custom/mem\"\n" +
		"state_db_path = \"/custom/state.db\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MemoryDir != "/custom/mem" || cfg.StateDBPath != "/custom/state.db" {
		t.Errorf("explicit paths not honored: mem=%q db=%q", cfg.MemoryDir, cfg.StateDBPath)
	}
}
```
Ensure `config_test.go`'s import block has `"os"` and `"path/filepath"` (add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad -v`
Expected: FAIL — `cfg.MemoryDir`/`cfg.StateDBPath` undefined (compile error).

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add two fields to the `Config` struct (after `TootPersonaPath`):
```go
	// MemoryDir holds the curated-memory files USER.md and MEMORY.md that are
	// injected into every prompt. Defaults to <dir of session_id_path>/memory.
	MemoryDir string `toml:"memory_dir"`
	// StateDBPath is the SQLite database holding the conversation turn log
	// (for session_search). Defaults to <dir of session_id_path>/state.db.
	StateDBPath string `toml:"state_db_path"`
```
Add `"path/filepath"` to the import block. Then, in `Load`, after `validate()` succeeds and before `return &cfg, nil`, apply defaults:
```go
	base := filepath.Dir(cfg.SessionIDPath)
	if cfg.MemoryDir == "" {
		cfg.MemoryDir = filepath.Join(base, "memory")
	}
	if cfg.StateDBPath == "" {
		cfg.StateDBPath = filepath.Join(base, "state.db")
	}
```
(These are optional fields — `validate()` is unchanged, so old configs keep working; the defaults fill them in.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS (new tests + all existing config tests).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): memory_dir + state_db_path with session-path defaults"
```

---

## Task 2: cmd/otto memory.go helpers

**Files:**
- Create: `cmd/otto/memory.go`
- Test: `cmd/otto/memory_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/otto/memory_test.go`:
```go
//go:build unix

package main

import (
	"context"
	"strings"
	"testing"

	"otto/internal/memory"
	"otto/internal/store"
)

func TestComposeMemoryPromptNilCoreReturnsBase(t *testing.T) {
	if got := composeMemoryPrompt("BASE", nil); got != "BASE" {
		t.Fatalf("nil core should return base unchanged, got %q", got)
	}
}

func TestComposeMemoryPromptEmptyCoreReturnsBase(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375) // no files written → empty
	if got := composeMemoryPrompt("BASE", c); got != "BASE" {
		t.Fatalf("empty core should return base unchanged, got %q", got)
	}
}

func TestComposeMemoryPromptAppendsCore(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetUser, "User is named Justin."); err != nil {
		t.Fatal(err)
	}
	got := composeMemoryPrompt("BASE PROMPT", c)
	if !strings.HasPrefix(got, "BASE PROMPT") {
		t.Errorf("base should come first: %q", got)
	}
	if !strings.Contains(got, "Justin") {
		t.Errorf("memory block missing: %q", got)
	}
}

func TestComposeMemoryPromptEmptyBaseReturnsBlockOnly(t *testing.T) {
	c := memory.NewCore(t.TempDir(), 2200, 1375)
	if err := c.Add(memory.TargetMemory, "Server runs Arch."); err != nil {
		t.Fatal(err)
	}
	got := composeMemoryPrompt("", c)
	if strings.HasPrefix(got, "\n") {
		t.Errorf("empty base should not leave a leading separator: %q", got)
	}
	if !strings.Contains(got, "Arch") {
		t.Errorf("memory block missing: %q", got)
	}
}

func TestLogTurnPersistsAndIsSearchable(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, "otto", "user", "remember the Tokyo trip")
	turns, err := st.SearchFTS(ctx, "Tokyo", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 logged turn, got %d", len(turns))
	}
}

func TestLogTurnNilStoreIsNoop(t *testing.T) {
	// Must not panic.
	logTurn(context.Background(), nil, "otto", "user", "anything")
}

func TestLogTurnSkipsBlankContent(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, "otto", "user", "   ")
	turns, err := st.SearchFTS(ctx, "anything", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 0 {
		t.Fatalf("blank content should not be logged, got %d turns", len(turns))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto/ -run 'TestComposeMemory|TestLogTurn' -v`
Expected: FAIL — `undefined: composeMemoryPrompt` / `undefined: logTurn`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/otto/memory.go`:
```go
//go:build unix

package main

import (
	"context"
	"log"
	"strings"

	"otto/internal/memory"
	"otto/internal/store"
)

// Memory core character caps (rough token proxies). Mirror the values in
// cmd/otto-memory; they only bound writes (which happen via the MCP server),
// so for Otto's read-only Inject they are immaterial but kept consistent.
const (
	memCapChars  = 2200
	userCapChars = 1375
)

// composeMemoryPrompt appends the curated memory core to a base system prompt.
// Returns base unchanged when core is nil or empty. The injected block carries
// its own header (see memory.Core.Inject).
func composeMemoryPrompt(base string, core *memory.Core) string {
	if core == nil {
		return base
	}
	block, err := core.Inject()
	if err != nil {
		log.Printf("memory inject: %v", err)
		return base
	}
	if block == "" {
		return base
	}
	if base == "" {
		return block
	}
	return base + "\n\n" + block
}

// logTurn appends one conversation turn to the store, best-effort. A nil store
// or blank content is a no-op. Errors are logged, never propagated — turn
// logging must never break a reply.
func logTurn(ctx context.Context, st *store.Store, persona, role, content string) {
	if st == nil || strings.TrimSpace(content) == "" {
		return
	}
	if _, err := st.AppendTurn(ctx, persona, role, content); err != nil {
		log.Printf("turn log (%s/%s): %v", persona, role, err)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto/ -run 'TestComposeMemory|TestLogTurn' -v`
Expected: PASS (all subtests).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/memory.go cmd/otto/memory_test.go
git commit -m "feat(otto): composeMemoryPrompt + logTurn helpers"
```

---

## Task 3: Inject + log in Otto's path (handler.go)

**Files:**
- Modify: `cmd/otto/handler.go`
- Modify: `cmd/otto/handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto/handler_test.go`:
```go
func TestHandlerInjectsMemoryAndLogsTurns(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "what's my flight"}}},
	}
	runner := &fakeRunner{respond: "your flight is at 9am"}
	h := newTestHandler(t, bot, runner)

	// Wire a memory core with one fact + a store for turn logging.
	dir := t.TempDir()
	core := memory.NewCore(dir, 2200, 1375)
	if err := core.Add(memory.TargetUser, "User flies to Tokyo on Friday."); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h.mem = core
	h.store = st
	h.baseSystemPrompt = "BASE PERSONA"

	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	asp := runner.called[0].AppendSystemPrompt
	if !strings.Contains(asp, "BASE PERSONA") || !strings.Contains(asp, "Tokyo on Friday") {
		t.Errorf("AppendSystemPrompt missing base or memory: %q", asp)
	}
	// Both the user message and the assistant reply should be logged.
	ctx := context.Background()
	if got, _ := st.SearchFTS(ctx, "flight", 5); len(got) == 0 {
		t.Error("user turn not logged")
	}
	if got, _ := st.SearchFTS(ctx, "9am", 5); len(got) == 0 {
		t.Error("assistant turn not logged")
	}
}
```
Add `"otto/internal/memory"` and `"otto/internal/store"` to `handler_test.go`'s import block (it already imports `"path/filepath"`, `"strings"`, `"context"`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto/ -run TestHandlerInjectsMemory -v`
Expected: FAIL — `h.mem` / `h.store` / `h.baseSystemPrompt` undefined.

- [ ] **Step 3: Write minimal implementation**

(a) In `cmd/otto/handler.go`, add fields to the `handler` struct (after `pets`):
```go
	mem              *memory.Core // injected into every Otto prompt; nil disables
	store            *store.Store // turn log for session_search; nil disables
	baseSystemPrompt string       // Otto's persona+footer prompt, before memory
```
Add imports `"otto/internal/memory"` and `"otto/internal/store"` to `handler.go`.

(b) In `handleMessage`, change the `runAndReply` call to inject memory. Replace:
```go
	h.runAndReply(callCtx, ctx, u.ChatID, claude.RunArgs{
		Prompt:     u.Text,
		SessionID:  h.session.ID(),
		ImagePaths: imagePaths,
	})
```
with:
```go
	h.runAndReply(callCtx, ctx, u.ChatID, claude.RunArgs{
		Prompt:             u.Text,
		SessionID:          h.session.ID(),
		ImagePaths:         imagePaths,
		AppendSystemPrompt: composeMemoryPrompt(h.baseSystemPrompt, h.mem),
	})
```

(c) In `runAndReply`, after the successful send block (the final `if err := telegram.SendChunked(sendCtx, h.bot, chatID, out); err != nil { ... }`), log the turns. Add immediately after that `if` block (and before the `permission_denials` handling, or after it — either is fine; place it right after the send `if`):
```go
	logTurn(sendCtx, h.store, "otto", "user", args.Prompt)
	logTurn(sendCtx, h.store, "otto", "assistant", out)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto/ -run TestHandler -v`
Expected: PASS (the new test + all existing handler tests — existing tests have `mem`/`store` nil so injection yields `""` and logging is a no-op, unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/handler.go cmd/otto/handler_test.go
git commit -m "feat(otto): inject memory core + log turns on Otto's path"
```

---

## Task 4: Inject + log in Toto and Toot

**Files:**
- Modify: `cmd/otto/toto.go`
- Modify: `cmd/otto/toot.go`
- Modify: `cmd/otto/toto_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto/toto_test.go` (it is in `package main`; reuse `fakeBot`/`fakeRunner` from handler_test.go):
```go
func TestTotoInjectsMemoryAndLogsTurn(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{respond: "mrow"}
	dir := t.TempDir()
	core := memory.NewCore(dir, 2200, 1375)
	if err := core.Add(memory.TargetUser, "User prefers brevity."); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sess, err := claude.LoadSession(filepath.Join(dir, "toto-sid"))
	if err != nil {
		t.Fatal(err)
	}
	toto := &Toto{
		bot:     bot,
		runner:  runner,
		session: sess,
		persona: "CAT PERSONA",
		mem:     core,
		store:   st,
	}

	toto.Reply(context.Background(), 100, "hello")

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if !strings.Contains(runner.called[0].AppendSystemPrompt, "User prefers brevity") {
		t.Errorf("toto prompt missing injected memory: %q", runner.called[0].AppendSystemPrompt)
	}
	if got, _ := st.SearchFTS(context.Background(), "hello", 5); len(got) == 0 {
		t.Error("toto user turn not logged")
	}
}
```
Ensure `toto_test.go` imports `"context"`, `"strings"`, `"path/filepath"`, `"otto/internal/claude"`, `"otto/internal/memory"`, `"otto/internal/store"` (add any missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto/ -run TestTotoInjects -v`
Expected: FAIL — `Toto` has no field `mem`/`store`.

- [ ] **Step 3: Write minimal implementation**

(a) `cmd/otto/toto.go` — add to the `Toto` struct (after `persona`):
```go
	mem   *memory.Core // injected into Toto's prompt; nil disables
	store *store.Store // turn log; nil disables
```
Add imports `"otto/internal/memory"` and `"otto/internal/store"` to `toto.go`.

In `replyWithContext`, just before the `events := make(chan claude.Event, 32)` line (i.e. after the full `systemPrompt` has been assembled), inject memory:
```go
	systemPrompt = composeMemoryPrompt(systemPrompt, t.mem)
```
Then after the final `t.send(ctx, chatID, out)` call near the end of `replyWithContext`, log the turns:
```go
	logTurn(ctx, t.store, "toto", "user", userMessage)
	logTurn(ctx, t.store, "toto", "assistant", out)
```

(b) `cmd/otto/toot.go` — add to the `Toot` struct (after `persona`):
```go
	mem   *memory.Core // injected into Toot's prompt; nil disables
	store *store.Store // turn log; nil disables
```
Add imports `"otto/internal/memory"` and `"otto/internal/store"` to `toot.go`.

In `Reply`, just before its `events := make(chan claude.Event, 32)` line (after `systemPrompt` is fully assembled), inject:
```go
	systemPrompt = composeMemoryPrompt(systemPrompt, t.mem)
```
Then after the final `t.deliver(ctx, chatID, out)` (the one that delivers the normal reply, just before the `if shouldTrigger` block), log:
```go
	logTurn(ctx, t.store, "toot", "user", userMessage)
	logTurn(ctx, t.store, "toot", "assistant", out)
```
(Do NOT add injection/logging to `Toot.Announce` or `Toot.Confirm` — those are release-event messages, not conversational turns, and Announce's prompt is the changelog, not chat.)

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto/ -v`
Expected: PASS (new Toto test + all existing tests; existing Toto/Toot construction leaves `mem`/`store` nil → unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/toto.go cmd/otto/toot.go cmd/otto/toto_test.go
git commit -m "feat(otto): inject memory + log turns in Toto and Toot"
```

---

## Task 5: Wire store + core in main.go

**Files:**
- Modify: `cmd/otto/main.go`

- [ ] **Step 1: Implementation (no new unit test — covered by build + the component tests above; main is the daemon entrypoint)**

In `cmd/otto/main.go`:

(a) Add imports `"otto/internal/memory"` and `"otto/internal/store"` (and ensure `"path/filepath"` is imported — add if missing).

(b) After the `runner := claude.NewExecRunner(...)` line (Otto's runner) and before the Toto wiring, open the store and build the core:
```go
	// Open the conversation turn-log store. Its parent dir may not exist yet
	// (store.Open creates the file, not the directory), so ensure it first.
	if err := os.MkdirAll(filepath.Dir(cfg.StateDBPath), 0700); err != nil {
		log.Fatalf("state db dir: %v", err)
	}
	memStore, err := store.Open(cfg.StateDBPath)
	if err != nil {
		log.Fatalf("open state db: %v", err)
	}
	defer memStore.Close()

	// Curated memory core, injected into every persona's prompt and written
	// via the otto-memory MCP server.
	memCore := memory.NewCore(cfg.MemoryDir, memCapChars, userCapChars)
```

(c) Where the `toto` and `toot` structs are constructed, add the `mem`/`store` fields:
```go
	toto := &Toto{
		bot:     bot,
		runner:  totoRunner,
		session: totoSession,
		persona: totoPersona,
		mem:     memCore,
		store:   memStore,
	}
	toot := &Toot{
		bot:     bot,
		runner:  tootRunner,
		session: tootSession,
		persona: tootPersona,
		mem:     memCore,
		store:   memStore,
	}
```

(d) Where the `h := &handler{...}` struct is constructed, add the three new fields:
```go
		mem:              memCore,
		store:            memStore,
		baseSystemPrompt: systemPrompt,
```
(`systemPrompt` is the existing local var from `buildSystemPrompt`.)

(e) Update the startup log line to mention memory wiring (optional but helpful) — append `memory_dir=%s state_db=%s` with `cfg.MemoryDir, cfg.StateDBPath` to the existing `log.Printf("otto: starting; ...")` format string and args.

- [ ] **Step 2: Build + vet**

Run:
```bash
go build ./...
go vet ./...
```
Expected: exit 0.

- [ ] **Step 3: Confirm the whole suite still passes**

Run: `go test ./...`
Expected: all packages pass.

- [ ] **Step 4: Commit**

```bash
git add cmd/otto/main.go
git commit -m "feat(otto): open state store + build memory core in main, wire personas"
```

---

## Task 6: setup.sh — build otto-memory + register in mcp.json + config paths

**Files:**
- Modify: `setup.sh`

- [ ] **Step 1: Add the otto-memory build and path vars**

In `setup.sh`, find the build block:
```bash
echo "  Building otto binary..."
go build -o "$OTTO_BIN" ./cmd/otto
echo "  [ok] $OTTO_BIN"
```
Add right after it:
```bash
OTTO_MEMORY_BIN="$OTTO_BIN_DIR/otto-memory"
echo "  Building otto-memory MCP server..."
go build -o "$OTTO_MEMORY_BIN" ./cmd/otto-memory
echo "  [ok] $OTTO_MEMORY_BIN"

# Memory storage locations (live under the state dir, alongside session ids).
OTTO_MEMORY_DIR="$OTTO_STATE_DIR/memory"
OTTO_STATE_DB="$OTTO_STATE_DIR/state.db"
mkdir -p "$OTTO_MEMORY_DIR"
```
(`OTTO_STATE_DIR` is created earlier in the script for session files; `mkdir -p` on the memory subdir is safe/idempotent.)

- [ ] **Step 2: Write the new config fields**

Find the config-field writes (near `write_toml_field session_id_path ...`). After the `session_id_path` line, add:
```bash
write_toml_field memory_dir "$OTTO_MEMORY_DIR" "$CONFIG_FILE"
write_toml_field state_db_path "$OTTO_STATE_DB" "$CONFIG_FILE"
```

- [ ] **Step 3: Register otto-memory in mcp.json**

In the inline python heredoc that builds `mcp.json` (`python3 - ... <<'PYEOF'`), the script passes values via environment variables. Add `OTTO_MEMORY_BIN`, `OTTO_MEMORY_DIR`, `OTTO_STATE_DB` to the env prefix of that `python3` invocation. Change:
```bash
NOTION_TOKEN_VAL="$NOTION_TOKEN" \
CLIENT_SECRET_FILE="$CLIENT_SECRET_FILE" \
DESKTOP_CLIENT_ID="$DESKTOP_CLIENT_ID" \
DESKTOP_CLIENT_SECRET="$DESKTOP_CLIENT_SECRET" \
GMAIL_OAUTH_PATH="$GMAIL_OAUTH_PATH" \
HOME_DIR="$HOME" \
python3 - "${GMAIL_LABELS[@]}" > "$MCP_FILE" <<'PYEOF'
```
to add three lines before `python3`:
```bash
NOTION_TOKEN_VAL="$NOTION_TOKEN" \
CLIENT_SECRET_FILE="$CLIENT_SECRET_FILE" \
DESKTOP_CLIENT_ID="$DESKTOP_CLIENT_ID" \
DESKTOP_CLIENT_SECRET="$DESKTOP_CLIENT_SECRET" \
GMAIL_OAUTH_PATH="$GMAIL_OAUTH_PATH" \
OTTO_MEMORY_BIN="$OTTO_MEMORY_BIN" \
OTTO_MEMORY_DIR="$OTTO_MEMORY_DIR" \
OTTO_STATE_DB="$OTTO_STATE_DB" \
HOME_DIR="$HOME" \
python3 - "${GMAIL_LABELS[@]}" > "$MCP_FILE" <<'PYEOF'
```
Then inside the python body, after the `config = {"mcpServers": {}}` line, add the otto-memory server entry:
```python
config["mcpServers"]["otto-memory"] = {
    "command": os.environ['OTTO_MEMORY_BIN'],
    "args": [
        "--memory-dir", os.environ['OTTO_MEMORY_DIR'],
        "--state-db", os.environ['OTTO_STATE_DB'],
    ],
}
```

- [ ] **Step 2 check / Step 4: Verify the script is syntactically valid and the mcp.json snippet emits the server**

Run a bash syntax check:
```bash
bash -n setup.sh && echo "setup.sh syntax OK"
```
Expected: `setup.sh syntax OK`.

Then verify the python snippet produces valid JSON containing the otto-memory entry (run the heredoc body standalone with fake env):
```bash
OTTO_MEMORY_BIN=/fake/otto-memory OTTO_MEMORY_DIR=/fake/mem OTTO_STATE_DB=/fake/state.db \
NOTION_TOKEN_VAL= CLIENT_SECRET_FILE=/x DESKTOP_CLIENT_ID=x DESKTOP_CLIENT_SECRET=x \
GMAIL_OAUTH_PATH=/x HOME_DIR="$HOME" python3 -c '
import json,os
config={"mcpServers":{}}
config["mcpServers"]["otto-memory"]={"command":os.environ["OTTO_MEMORY_BIN"],"args":["--memory-dir",os.environ["OTTO_MEMORY_DIR"],"--state-db",os.environ["OTTO_STATE_DB"]]}
print(json.dumps(config,indent=2))
' | python3 -c 'import json,sys; d=json.load(sys.stdin); assert "otto-memory" in d["mcpServers"]; assert d["mcpServers"]["otto-memory"]["command"]=="/fake/otto-memory"; print("mcp.json otto-memory entry OK")'
```
Expected: `mcp.json otto-memory entry OK`. (This validates the exact dict shape you added to the real heredoc.)

- [ ] **Step 5: Commit**

```bash
git add setup.sh
git commit -m "feat(setup): build otto-memory, register in mcp.json, write memory config paths"
```

---

## Task 7: Final verification

- [ ] **Step 1: Vet + format + build + test (+ race on touched packages)**

Run:
```bash
go vet ./...
gofmt -l cmd/otto/ internal/config/
go build ./...
go test ./...
go test -race ./cmd/otto/ ./internal/config/
```
Expected: vet exit 0; gofmt prints nothing; build exit 0; all tests pass; no races.

- [ ] **Step 2: Confirm the otto-memory binary still builds and the bot binary builds with memory wired**

Run:
```bash
go build -o /tmp/otto ./cmd/otto && echo "otto OK"
go build -o /tmp/otto-memory ./cmd/otto-memory && echo "otto-memory OK"
rm -f /tmp/otto /tmp/otto-memory
```
Expected: both print OK.

- [ ] **Step 3: Sanity-check the integration seam**

Confirm the three personas all reference `composeMemoryPrompt` and the store is closed on shutdown:
```bash
grep -n "composeMemoryPrompt" cmd/otto/*.go
grep -n "memStore.Close\|defer memStore" cmd/otto/main.go
```
Expected: `composeMemoryPrompt` appears in handler.go, toto.go, toot.go (and memory.go); `memStore.Close` deferred in main.go.

---

## Self-Review notes

- **Spec coverage (this plan's slice):** memory core injected into all three personas (Tasks 3–4, via Task 2 helper); turn logging to state.db for all three (Tasks 3–4); store + core constructed and closed in main (Task 5); config paths with defaults (Task 1); otto-memory registered in mcp.json so the memory tools are callable + memory dir created + state-db parent ensured (Tasks 5–6). Deferred to Plan 4 (correctly out of scope): embedder chain + semantic retrieval, idle-gated session rotation + token capture, default USER.md/MEMORY.md seeding (optional polish).
- **Backward compatibility:** every new field is nil-tolerant — `composeMemoryPrompt(base, nil)==base`, `logTurn(_, nil, …)` is a no-op — so all existing `cmd/otto` tests (which build handlers/personas without `mem`/`store`) pass unchanged.
- **Type/signature consistency:** `composeMemoryPrompt(string, *memory.Core) string` and `logTurn(context.Context, *store.Store, string, string, string)` defined in Task 2, used identically in Tasks 3–5. Fields `mem *memory.Core`, `store *store.Store` added to `handler`, `Toto`, `Toot`; `handler.baseSystemPrompt string`. Uses Plan-1/2 APIs exactly: `memory.NewCore`, `Core.Inject`, `memory.TargetUser/TargetMemory`, `store.Open/Close/AppendTurn/SearchFTS`. Config fields `MemoryDir`/`StateDBPath` match the `setup.sh` TOML keys `memory_dir`/`state_db_path` and the otto-memory flags `--memory-dir`/`--state-db`.
- **Seam correctness:** `setup.sh` points the otto-memory MCP server at `$OTTO_STATE_DIR/memory` and `$OTTO_STATE_DIR/state.db`, the SAME paths config defaults to (since `session_id_path` is `$OTTO_STATE_DIR/session_id`, its dir is `$OTTO_STATE_DIR`). So the core Otto injects and the core the MCP tools write are one and the same, and the turns Otto logs are what session_search reads.
```
