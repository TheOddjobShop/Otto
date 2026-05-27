# Otto Semantic Wiring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Activate semantic memory retrieval end-to-end: build the `embed.Chain` from config in both binaries, embed each conversation turn on log (bot → `PutVector`), and make the otto-memory `session_search` tool merge semantic results (`SearchSemantic`) with keyword results (`SearchFTS`), falling back to keyword-only when embedding is unavailable.

**Architecture:** A new `embed.NewOllamaChain(baseURL, models)` constructs the ordered fallthrough chain. Config gains `embed_ollama_url` + `embed_models`. The bot builds one shared `embed.Embedder` and hands it to the handler/Toto/Toot; `logTurn` appends the turn then asynchronously embeds it and stores the vector (best-effort, off the reply path, 30s-bounded). The otto-memory server builds its own chain from new flags and its `session_search` embeds the query, runs semantic + FTS in parallel, and merges/dedupes by turn ID (semantic first, keyword fill), degrading to FTS-only when the embed chain errors. Everything is nil-tolerant: no embedder → today's keyword-only behavior, unchanged.

**Tech Stack:** Go 1.26, existing `internal/embed`, `internal/store`, `internal/config`, `cmd/otto`, `cmd/otto-memory`. No new dependencies. (Ollama install/pull in `setup.sh` is a separate follow-up; the chain degrades to keyword search if Ollama is absent, so this plan is safe without it.)

---

## Context on existing code

- `internal/embed`: `Embedder` interface (`Embed(ctx,string)(Result,error)`, `Name()string`); `Result{Vector []float32; Model string}`; `NewOllama(baseURL, model) *Ollama`; `NewChain(...Embedder) *Chain` (tries in order, first success; all-fail → error); `Cosine`.
- `internal/store`: `AppendTurn(ctx,persona,role,content)(int64,error)`, `SearchFTS(ctx,query,limit)([]Turn,error)`, `SearchSemantic(ctx,query []float32,limit)([]Turn,error)`, `PutVector(ctx,turnID int64,model string,vec []float32)error`, `Turn{ID,Persona,Role,Content,TS}`.
- `internal/config`: `Config` with `Load` that applies defaults after `validate()`. Has `MemoryDir`, `StateDBPath`.
- `cmd/otto/memory.go`: `composeMemoryPrompt`, `logTurn(ctx, st *store.Store, persona, role, content string)`, consts `memCapChars/userCapChars`. `handler`/`Toto`/`Toot` have `mem *memory.Core` + `store *store.Store`; they call `logTurn(... )` (handler uses `sendCtx`; Toto/Toot use `ctx`). `main.go` builds `memStore`/`memCore` and wires them.
- `cmd/otto-memory/server.go`: `memoryServer{core *memory.Core; store *store.Store}`, `handleSearch(ctx, req, searchArgs)` calls `s.store.SearchFTS` then formats with `truncateContent`. `main.go` has `--memory-dir`/`--state-db` flags + builds the server + registers tools.

## File Structure

- `internal/embed/chain.go` (modify) — add `NewOllamaChain`.
- `internal/embed/chain_test.go` (modify) — test it.
- `internal/config/config.go` (modify) — `EmbedOllamaURL`, `EmbedModels` + defaults.
- `internal/config/config_test.go` (modify) — defaults test.
- `cmd/otto/memory.go` (modify) — `embedAndStore`; `logTurn` gains an embedder param + async embed.
- `cmd/otto/{handler,toto,toot}.go` (modify) — add `embedder embed.Embedder` field; pass to `logTurn`.
- `cmd/otto/memory_test.go` (modify) — `embedAndStore` test + updated `logTurn` calls.
- `cmd/otto/handler_test.go` / `toto_test.go` (modify) — updated `logTurn` call sites (the test that wires store).
- `cmd/otto/main.go` (modify) — build chain, set `embedder` on handler/Toto/Toot.
- `cmd/otto-memory/server.go` (modify) — `embedder` field; `handleSearch` semantic+FTS merge; `mergeTurns` helper.
- `cmd/otto-memory/server_test.go` (modify) — merge + fallback tests.
- `cmd/otto-memory/main.go` (modify) — `--embed-url`/`--embed-models` flags, build chain, wire.

---

## Task 1: embed.NewOllamaChain

**Files:**
- Modify: `internal/embed/chain.go`
- Modify: `internal/embed/chain_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/embed/chain_test.go`:
```go
func TestNewOllamaChainBuildsBackendsInOrder(t *testing.T) {
	c := NewOllamaChain("http://localhost:11434", []string{"embeddinggemma", "nomic-embed-text"})
	name := c.Name()
	if !strings.Contains(name, "ollama:embeddinggemma") || !strings.Contains(name, "ollama:nomic-embed-text") {
		t.Fatalf("chain name missing backends: %q", name)
	}
	// embeddinggemma must come before nomic (ordered fallthrough).
	if strings.Index(name, "embeddinggemma") > strings.Index(name, "nomic-embed-text") {
		t.Errorf("expected embeddinggemma before nomic: %q", name)
	}
}

func TestNewOllamaChainSkipsBlankModels(t *testing.T) {
	c := NewOllamaChain("http://x", []string{"", "embeddinggemma", "  "})
	// Only the one real model should be present.
	if strings.Count(c.Name(), "ollama:") != 1 {
		t.Errorf("blank models should be skipped: %q", c.Name())
	}
}
```
(`strings` is already imported in chain_test.go.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/embed/ -run TestNewOllamaChain -v`
Expected: FAIL — `undefined: NewOllamaChain`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/embed/chain.go`:
```go
// NewOllamaChain builds a Chain of Ollama backends, one per model, all hitting
// the same baseURL, tried in the given order. Blank/whitespace model names are
// skipped. With no usable models the chain is empty and Embed will error
// (caller falls back to keyword search).
func NewOllamaChain(baseURL string, models []string) *Chain {
	backends := make([]Embedder, 0, len(models))
	for _, m := range models {
		if strings.TrimSpace(m) == "" {
			continue
		}
		backends = append(backends, NewOllama(baseURL, m))
	}
	return NewChain(backends...)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/embed/ -v`
Expected: PASS (whole package).

- [ ] **Step 5: Commit**

```bash
git add internal/embed/chain.go internal/embed/chain_test.go
git commit -m "feat(embed): NewOllamaChain constructor from baseURL + models"
```

---

## Task 2: config embed_ollama_url + embed_models

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:
```go
func TestLoadDerivesEmbedDefaults(t *testing.T) {
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
		"session_id_path = \"" + dir + "/session_id\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbedOllamaURL != "http://localhost:11434" {
		t.Errorf("EmbedOllamaURL default = %q", cfg.EmbedOllamaURL)
	}
	if len(cfg.EmbedModels) != 2 || cfg.EmbedModels[0] != "embeddinggemma" || cfg.EmbedModels[1] != "nomic-embed-text" {
		t.Errorf("EmbedModels default = %v", cfg.EmbedModels)
	}
}

func TestLoadHonorsExplicitEmbedConfig(t *testing.T) {
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
		"embed_ollama_url = \"http://ollama:9999\"\n" +
		"embed_models = [\"only-model\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbedOllamaURL != "http://ollama:9999" {
		t.Errorf("explicit url not honored: %q", cfg.EmbedOllamaURL)
	}
	if len(cfg.EmbedModels) != 1 || cfg.EmbedModels[0] != "only-model" {
		t.Errorf("explicit models not honored: %v", cfg.EmbedModels)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad.*Embed -v`
Expected: FAIL — `cfg.EmbedOllamaURL`/`cfg.EmbedModels` undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add two fields to `Config` (after `StateDBPath`):
```go
	// EmbedOllamaURL is the base URL of the local Ollama server used for
	// semantic-memory embeddings. Default http://localhost:11434.
	EmbedOllamaURL string `toml:"embed_ollama_url"`
	// EmbedModels is the ordered list of Ollama embedding models to try
	// (first healthy wins). Default ["embeddinggemma", "nomic-embed-text"].
	EmbedModels []string `toml:"embed_models"`
```
In `Load`, after the existing memory-path defaults block, add:
```go
	if cfg.EmbedOllamaURL == "" {
		cfg.EmbedOllamaURL = "http://localhost:11434"
	}
	if len(cfg.EmbedModels) == 0 {
		cfg.EmbedModels = []string{"embeddinggemma", "nomic-embed-text"}
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS (new + existing).

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): embed_ollama_url + embed_models with defaults"
```

---

## Task 3: bot embeds turns on log

**Files:**
- Modify: `cmd/otto/memory.go`
- Modify: `cmd/otto/memory_test.go`
- Modify: `cmd/otto/handler.go`, `cmd/otto/toto.go`, `cmd/otto/toot.go` (logTurn call sites + embedder field)
- Modify: `cmd/otto/handler_test.go`, `cmd/otto/toto_test.go` (call-site fixups)

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto/memory_test.go`:
```go
// fakeEmbedder returns a fixed vector for any text.
type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(ctx context.Context, text string) (embed.Result, error) {
	return embed.Result{Vector: f.vec, Model: "fake"}, nil
}
func (f fakeEmbedder) Name() string { return "fake" }

func TestEmbedAndStorePersistsVector(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	id, err := st.AppendTurn(ctx, "otto", "user", "the Tokyo trip")
	if err != nil {
		t.Fatal(err)
	}
	embedAndStore(st, fakeEmbedder{vec: []float32{1, 0}}, id, "the Tokyo trip")

	// The stored vector should be findable via semantic search.
	got, err := st.SearchSemantic(ctx, []float32{1, 0}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("embedded turn not searchable: %+v", got)
	}
}

func TestLogTurnWithEmbedderStillLogsTurn(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	logTurn(ctx, st, fakeEmbedder{vec: []float32{1, 0}}, "otto", "user", "hello tokyo")
	if got, _ := st.SearchFTS(ctx, "tokyo", 5); len(got) == 0 {
		t.Fatal("turn not logged")
	}
}
```
Add `"otto/internal/embed"` to `memory_test.go`'s import block (it already imports `context`, `store`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto/ -run 'TestEmbedAndStore|TestLogTurnWithEmbedder' -v`
Expected: FAIL — `undefined: embedAndStore`, and `logTurn` arity mismatch (it takes the new embedder param).

- [ ] **Step 3: Write minimal implementation**

(a) In `cmd/otto/memory.go`, add `"time"` and `"otto/internal/embed"` to the import block. Add `embedAndStore` and change `logTurn`:
```go
// embedAndStore embeds content and persists the vector for turnID, best-effort.
// Synchronous and 30s-bounded; callers run it in a goroutine off the reply
// path. Errors are logged, never propagated.
func embedAndStore(st *store.Store, emb embed.Embedder, turnID int64, content string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := emb.Embed(ctx, content)
	if err != nil {
		log.Printf("embed turn %d: %v", turnID, err)
		return
	}
	if err := st.PutVector(ctx, turnID, r.Model, r.Vector); err != nil {
		log.Printf("put vector %d: %v", turnID, err)
	}
}

// logTurn appends one conversation turn (best-effort) and, when emb is non-nil,
// asynchronously embeds it for semantic search. A nil store or blank content is
// a no-op. Turn logging must never break a reply, so embedding runs in a
// detached goroutine.
func logTurn(ctx context.Context, st *store.Store, emb embed.Embedder, persona, role, content string) {
	if st == nil || strings.TrimSpace(content) == "" {
		return
	}
	id, err := st.AppendTurn(ctx, persona, role, content)
	if err != nil {
		log.Printf("turn log (%s/%s): %v", persona, role, err)
		return
	}
	if emb != nil {
		go embedAndStore(st, emb, id, content)
	}
}
```

(b) Add an `embedder embed.Embedder` field:
- `cmd/otto/handler.go` — in the `handler` struct after `store`:
  ```go
	embedder embed.Embedder // embeds turns for semantic search; nil disables
  ```
  Add `"otto/internal/embed"` to handler.go imports.
- `cmd/otto/toto.go` — in `Toto` after `store`:
  ```go
	embedder embed.Embedder // embeds turns for semantic search; nil disables
  ```
  Add `"otto/internal/embed"` to toto.go imports.
- `cmd/otto/toot.go` — in `Toot` after `store`:
  ```go
	embedder embed.Embedder // embeds turns for semantic search; nil disables
  ```
  Add `"otto/internal/embed"` to toot.go imports.

(c) Update the `logTurn` call sites to pass the embedder:
- `handler.go` `runAndReply` (the two calls):
  ```go
	logTurn(sendCtx, h.store, h.embedder, "otto", "user", args.Prompt)
	logTurn(sendCtx, h.store, h.embedder, "otto", "assistant", out)
  ```
- `toto.go` `replyWithContext` (the two calls):
  ```go
	logTurn(ctx, t.store, t.embedder, "toto", "user", userMessage)
	logTurn(ctx, t.store, t.embedder, "toto", "assistant", out)
  ```
- `toot.go` `Reply` (the two calls):
  ```go
	logTurn(ctx, t.store, t.embedder, "toot", "user", userMessage)
	logTurn(ctx, t.store, t.embedder, "toot", "assistant", out)
  ```

(d) Fix the existing test call site in `cmd/otto/handler_test.go` `TestHandlerInjectsMemoryAndLogsTurns` — it constructs `h` with `h.store = st` but no embedder, which is fine (nil embedder). No logTurn calls are made directly in tests, so no test-side `logTurn` signature fixups are needed beyond ensuring the package compiles. (If any test calls `logTurn` directly, add a `nil` embedder arg.) The new `memory_test.go` tests already use the new signature.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto/ -v`
Expected: PASS — new embed tests + all existing (existing handlers/personas leave `embedder` nil, so no embedding happens and behavior is unchanged).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/memory.go cmd/otto/memory_test.go cmd/otto/handler.go cmd/otto/toto.go cmd/otto/toot.go cmd/otto/handler_test.go cmd/otto/toto_test.go
git commit -m "feat(otto): embed turns on log for semantic search (async, best-effort)"
```

---

## Task 4: build the embed chain in cmd/otto main

**Files:**
- Modify: `cmd/otto/main.go`

- [ ] **Step 1: Implementation (covered by build + Task 3 tests; main is the entrypoint)**

In `cmd/otto/main.go`:
(a) Add `"otto/internal/embed"` to imports.
(b) After `memCore := memory.NewCore(...)`, build the chain:
```go
	// Embedder for semantic memory. Degrades to keyword search if Ollama is
	// unavailable (the chain returns an error and callers fall back to FTS).
	embedder := embed.NewOllamaChain(cfg.EmbedOllamaURL, cfg.EmbedModels)
```
(c) Add `embedder: embedder,` to the `toto` and `toot` struct literals and the `h := &handler{...}` literal (alongside the existing `mem`/`store` fields).
(d) Extend the startup log line with ` embed=%s` and `embedder.Name()`.

- [ ] **Step 2: Build + vet + test**

Run:
```bash
go build ./...
go vet ./...
go test ./...
```
Expected: all exit 0 / pass.

- [ ] **Step 3: Commit**

```bash
git add cmd/otto/main.go
git commit -m "feat(otto): build embed chain from config and wire into personas"
```

---

## Task 5: otto-memory session_search merges semantic + FTS

**Files:**
- Modify: `cmd/otto-memory/server.go`
- Modify: `cmd/otto-memory/server_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto-memory/server_test.go`:
```go
// fakeEmbedder returns a fixed vector regardless of input.
type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(ctx context.Context, text string) (embed.Result, error) {
	return embed.Result{Vector: f.vec, Model: "fake"}, nil
}
func (f fakeEmbedder) Name() string { return "fake" }

func TestHandleSearchMergesSemanticAndFTS(t *testing.T) {
	s := newTestServer(t)
	s.embedder = fakeEmbedder{vec: []float32{1, 0}}
	ctx := context.Background()
	// One turn matches by keyword, another only by vector similarity.
	kwID, _ := s.store.AppendTurn(ctx, "otto", "user", "keyword apple")
	semID, _ := s.store.AppendTurn(ctx, "otto", "assistant", "totally unrelated wording")
	if err := s.store.PutVector(ctx, semID, "fake", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	_ = kwID

	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "apple"})
	if err != nil || res.IsError {
		t.Fatalf("handleSearch: err=%v res=%q", err, resultText(res))
	}
	text := resultText(res)
	// Semantic hit (the vector-matched turn) and keyword hit should both appear.
	if !strings.Contains(text, "unrelated wording") {
		t.Errorf("semantic hit missing: %q", text)
	}
	if !strings.Contains(text, "keyword apple") {
		t.Errorf("keyword hit missing: %q", text)
	}
}

func TestHandleSearchNoEmbedderIsKeywordOnly(t *testing.T) {
	s := newTestServer(t) // embedder nil
	ctx := context.Background()
	if _, err := s.store.AppendTurn(ctx, "otto", "user", "keyword banana"); err != nil {
		t.Fatal(err)
	}
	res, _, err := s.handleSearch(ctx, nil, searchArgs{Query: "banana"})
	if err != nil || res.IsError {
		t.Fatalf("handleSearch: err=%v", err)
	}
	if !strings.Contains(resultText(res), "banana") {
		t.Errorf("keyword-only search failed: %q", resultText(res))
	}
}

func TestMergeTurnsDedupesByIDSemanticFirst(t *testing.T) {
	semantic := []store.Turn{{ID: 1, Content: "a"}, {ID: 2, Content: "b"}}
	fts := []store.Turn{{ID: 2, Content: "b"}, {ID: 3, Content: "c"}}
	got := mergeTurns(semantic, fts, 10)
	if len(got) != 3 {
		t.Fatalf("expected 3 unique, got %d: %+v", len(got), got)
	}
	// Semantic order preserved first: 1, 2, then FTS-only 3.
	if got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Errorf("merge order wrong: %+v", got)
	}
}

func TestMergeTurnsRespectsLimit(t *testing.T) {
	semantic := []store.Turn{{ID: 1}, {ID: 2}}
	fts := []store.Turn{{ID: 3}, {ID: 4}}
	got := mergeTurns(semantic, fts, 3)
	if len(got) != 3 {
		t.Fatalf("limit not respected: got %d", len(got))
	}
}
```
Add `"otto/internal/embed"` and `"otto/internal/store"` to `server_test.go`'s import block (it already imports `context`, `strings`, `testing`, `mcp`, `memory`).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto-memory/ -run 'TestHandleSearchMerges|TestHandleSearchNoEmbedder|TestMergeTurns' -v`
Expected: FAIL — `s.embedder` undefined, `mergeTurns` undefined.

- [ ] **Step 3: Write minimal implementation**

In `cmd/otto-memory/server.go`:
(a) Add `"otto/internal/embed"` and `"otto/internal/store"` to imports.
(b) Add an `embedder` field to `memoryServer`:
```go
type memoryServer struct {
	core     *memory.Core
	store    *store.Store
	embedder embed.Embedder // optional; nil = keyword-only search
}
```
(c) Replace `handleSearch` with the merged version:
```go
func (s *memoryServer) handleSearch(ctx context.Context, req *mcp.CallToolRequest, args searchArgs) (*mcp.CallToolResult, any, error) {
	limit := args.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	var semantic []store.Turn
	if s.embedder != nil {
		if r, err := s.embedder.Embed(ctx, args.Query); err == nil {
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
```
Add `"log"` to server.go's import block if not already present.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto-memory/ -v`
Expected: PASS — new merge tests + all existing (existing tests leave `embedder` nil → keyword-only, unchanged).

- [ ] **Step 5: Commit**

```bash
git add cmd/otto-memory/server.go cmd/otto-memory/server_test.go
git commit -m "feat(otto-memory): session_search merges semantic + keyword results"
```

---

## Task 6: otto-memory main builds the embed chain

**Files:**
- Modify: `cmd/otto-memory/main.go`

- [ ] **Step 1: Implementation (covered by build + smoke; main is the entrypoint)**

In `cmd/otto-memory/main.go`:
(a) Add `"strings"` and `"otto/internal/embed"` to imports.
(b) Add two flags alongside the existing ones:
```go
	embedURL := flag.String("embed-url", "http://localhost:11434", "Ollama base URL for semantic search embeddings")
	embedModels := flag.String("embed-models", "embeddinggemma,nomic-embed-text", "comma-separated Ollama embedding models, tried in order")
```
(c) After constructing `srv`, build and attach the embedder:
```go
	var models []string
	for _, m := range strings.Split(*embedModels, ",") {
		if s := strings.TrimSpace(m); s != "" {
			models = append(models, s)
		}
	}
	srv.embedder = embed.NewOllamaChain(*embedURL, models)
```
(Leave `srv.embedder` set to the chain even if Ollama is down — `session_search` catches embed errors and falls back to keyword.)

- [ ] **Step 2: Build + smoke test**

Run:
```bash
go build -o /tmp/otto-memory ./cmd/otto-memory && echo "build OK" && rm -f /tmp/otto-memory
```
Expected: `build OK`.

Then confirm the tool list still has the four tools (the embedder doesn't change the tool surface):
```bash
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' \
  | (go build -o /tmp/om ./cmd/otto-memory && sleep 0.2 && printf '' ; /tmp/om --memory-dir "$(mktemp -d)" --state-db "$(mktemp -d)/state.db" --embed-models "" ) 2>/dev/null | head -c 400; echo; rm -f /tmp/om
```
Expected: a JSON-RPC response containing the four tool names. (Passing `--embed-models ""` makes the chain empty so the smoke test needs no running Ollama; an empty chain just means semantic search no-ops and search is keyword-only.)

- [ ] **Step 3: Commit**

```bash
git add cmd/otto-memory/main.go
git commit -m "feat(otto-memory): --embed-url/--embed-models flags build the embed chain"
```

---

## Task 7: Final verification

- [ ] **Step 1: Vet + format + build + test (+ race on touched packages)**

Run:
```bash
go vet ./...
gofmt -l cmd/ internal/
go build ./...
go test ./...
go test -race ./cmd/otto/ ./cmd/otto-memory/ ./internal/embed/ ./internal/config/
```
Expected: vet 0; gofmt nothing; build 0; all tests pass; no races.

- [ ] **Step 2: Both binaries build**

Run:
```bash
go build -o /tmp/otto ./cmd/otto && go build -o /tmp/otto-memory ./cmd/otto-memory && echo "both OK" && rm -f /tmp/otto /tmp/otto-memory
```
Expected: `both OK`.

- [ ] **Step 3: Sanity-check the wiring**

Run:
```bash
grep -n "NewOllamaChain" cmd/otto/main.go cmd/otto-memory/main.go
grep -n "embedAndStore\|embedder" cmd/otto/memory.go
grep -n "mergeTurns\|s.embedder" cmd/otto-memory/server.go
```
Expected: `NewOllamaChain` in both mains; `embedAndStore`/`embedder` in memory.go; `mergeTurns`/`s.embedder` in server.go.

---

## Self-Review notes

- **Spec coverage (this slice):** embed chain from config in both binaries (Tasks 1,2,4,6); embed turns on log → PutVector, async/best-effort/off the reply path (Task 3); session_search merges semantic+FTS with keyword fallback (Task 5). Deferred: `setup.sh` Ollama install/pull + passing `--embed-url`/`--embed-models` from setup (separate follow-up — the flags default sensibly so the server works once Ollama is present); idle rotator + token capture (Plan 4d).
- **Type consistency:** `embed.NewOllamaChain(string, []string) *Chain`; `embedAndStore(*store.Store, embed.Embedder, int64, string)`; `logTurn(context.Context, *store.Store, embed.Embedder, string, string, string)` — signature changed (embedder added) and ALL call sites in handler/toto/toot updated in Task 3; `memoryServer.embedder embed.Embedder`; `mergeTurns([]store.Turn, []store.Turn, int) []store.Turn`. Config keys `embed_ollama_url`/`embed_models` match the otto-memory flag defaults and the bot's chain construction.
- **Backward compatibility / safety:** every embedder is nil-tolerant (bot: nil → no embedding; otto-memory: nil → keyword-only). The chain returns an error when Ollama is down, which both call sites catch → graceful FTS fallback. So this plan is safe to merge even before Ollama is installed.
- **Note for setup follow-up:** `setup.sh` should `write_toml_field embed_ollama_url`/`embed_models` (optional, defaults suffice) and pass `--embed-url`/`--embed-models` to the otto-memory entry in mcp.json, plus install Ollama and `ollama pull embeddinggemma nomic-embed-text`.
```
