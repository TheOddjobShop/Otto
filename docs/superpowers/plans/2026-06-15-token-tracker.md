# Token Tracker Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist a per-call token-usage breakdown (input/output/cache) for every Claude subprocess Otto spawns, attributed to its source, and expose it via a `/tokens` Telegram command.

**Architecture:** A new append-only `token_usage` SQLite table in the existing state DB. The stream-json parser is extended to surface output tokens and the raw input/cache fields (on top of the existing `ContextTokens` the rotator uses). A `Source` tag on `claude.RunArgs` plus a shared `recordUsage` helper write one row wherever a `ResultEvent` is observed (main, bus, Toto, Toot). The Haiku classifier is switched to JSON output so its usage is captured too. A `/tokens` command renders running totals + per-source breakdown.

**Tech Stack:** Go, `modernc.org/sqlite`, Claude Code CLI (`--output-format stream-json` / `json`), Telegram bot.

---

## File Structure

- `internal/store/store.go` — add `token_usage` to the idempotent schema.
- `internal/store/usage.go` (new) — `RecordUsage`, `UsageTotals`, `UsageBySource` + result types.
- `internal/store/usage_test.go` (new) — store round-trip tests.
- `internal/claude/event.go` — new raw-token fields on `ResultEvent`.
- `internal/claude/parser.go` — capture `output_tokens`, populate new fields.
- `internal/claude/parser_test.go` — assert new fields + `ContextTokens` regression.
- `internal/claude/runner.go` — `Source` field on `RunArgs` (metadata only).
- `cmd/otto/usage.go` (new) — `recordUsage` helper + `formatUsage` renderer.
- `cmd/otto/usage_test.go` (new) — `formatUsage` rendering test.
- `cmd/otto/handler.go` — record in `runAndReply`; set `Source: "main"`.
- `cmd/otto/bus.go` — set `Source: "bus"`.
- `cmd/otto/toto.go` — set `Source: "toto"`; record in event loop.
- `cmd/otto/toot.go` — set `Source: "toot"`; record in both event loops.
- `cmd/otto/classify.go` — JSON output; parse verdict + usage; record `"classify"`.
- `cmd/otto/classify_test.go` — JSON-envelope parse test.
- `cmd/otto/commands.go` — `/tokens` command.

**Note on timestamps:** `RecordUsage` stamps `ts` internally with `time.Now().Unix()`, matching the existing `AppendTurn` pattern in `internal/store/turns.go` (the design doc's `ts` parameter is dropped in favour of this established idiom).

---

### Task 1: Store layer — `token_usage` table + accessors

**Files:**
- Modify: `internal/store/store.go` (schema const, ~line 56)
- Create: `internal/store/usage.go`
- Create: `internal/store/usage_test.go`

- [ ] **Step 1: Add the table to the schema**

In `internal/store/store.go`, append to the `schema` string constant, just before the closing backtick (after the `inbox_undelivered` index):

```sql
CREATE TABLE IF NOT EXISTS token_usage (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	source          TEXT    NOT NULL,
	model           TEXT    NOT NULL,
	input_tokens    INTEGER NOT NULL,
	output_tokens   INTEGER NOT NULL,
	cache_creation  INTEGER NOT NULL,
	cache_read      INTEGER NOT NULL,
	ts              INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS token_usage_source ON token_usage(source);
```

- [ ] **Step 2: Write the failing test**

Create `internal/store/usage_test.go`:

```go
package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecordUsageAndAggregate(t *testing.T) {
	s := openTestStore(t)

	rows := []struct {
		source              string
		model               string
		in, out, cc, cr int
	}{
		{"main", "claude-opus-4-8", 100, 20, 5, 1000},
		{"main", "claude-sonnet-4-6", 50, 10, 0, 500},
		{"toto", "claude-haiku-4-5", 30, 5, 0, 200},
	}
	for _, r := range rows {
		if err := s.RecordUsage(r.source, r.model, r.in, r.out, r.cc, r.cr); err != nil {
			t.Fatalf("RecordUsage: %v", err)
		}
	}

	totals, err := s.UsageTotals()
	if err != nil {
		t.Fatalf("UsageTotals: %v", err)
	}
	if totals.InputTokens != 180 || totals.OutputTokens != 35 {
		t.Errorf("totals = %+v, want input 180 output 35", totals)
	}
	if totals.CacheCreation != 5 || totals.CacheRead != 1700 {
		t.Errorf("totals cache = %+v, want cc 5 cr 1700", totals)
	}

	bySrc, err := s.UsageBySource()
	if err != nil {
		t.Fatalf("UsageBySource: %v", err)
	}
	if len(bySrc) != 2 {
		t.Fatalf("got %d sources, want 2", len(bySrc))
	}
	// Ordered by source name: main, toto.
	if bySrc[0].Source != "main" || bySrc[0].InputTokens != 150 || bySrc[0].OutputTokens != 30 {
		t.Errorf("bySrc[0] = %+v, want main 150/30", bySrc[0])
	}
	if bySrc[1].Source != "toto" || bySrc[1].InputTokens != 30 {
		t.Errorf("bySrc[1] = %+v, want toto 30", bySrc[1])
	}
}

func TestUsageTotalsEmpty(t *testing.T) {
	s := openTestStore(t)
	totals, err := s.UsageTotals()
	if err != nil {
		t.Fatalf("UsageTotals: %v", err)
	}
	if totals.InputTokens != 0 || totals.OutputTokens != 0 {
		t.Errorf("empty totals = %+v, want all zero", totals)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/store/ -run TestRecordUsage -v`
Expected: FAIL — `s.RecordUsage undefined`.

- [ ] **Step 4: Implement the accessors**

Create `internal/store/usage.go`:

```go
package store

import (
	"fmt"
	"time"
)

// Totals is an aggregate of token_usage rows.
type Totals struct {
	InputTokens   int
	OutputTokens  int
	CacheCreation int
	CacheRead     int
}

// SourceTotals is Totals for one source label.
type SourceTotals struct {
	Source string
	Totals
}

// RecordUsage appends one token-usage row. It stamps ts with the current unix
// time, mirroring AppendTurn. Best-effort callers may ignore the error after
// logging — a failed usage write must never break a reply.
func (s *Store) RecordUsage(source, model string, in, out, cacheCreation, cacheRead int) error {
	_, err := s.db.Exec(
		`INSERT INTO token_usage(source, model, input_tokens, output_tokens, cache_creation, cache_read, ts)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		source, model, in, out, cacheCreation, cacheRead, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("store: record usage: %w", err)
	}
	return nil
}

// UsageTotals returns the grand total across all rows. Zero values on an empty
// table (COALESCE turns the NULL SUM of no rows into 0).
func (s *Store) UsageTotals() (Totals, error) {
	var t Totals
	err := s.db.QueryRow(`
		SELECT COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0), COALESCE(SUM(cache_read), 0)
		FROM token_usage`).
		Scan(&t.InputTokens, &t.OutputTokens, &t.CacheCreation, &t.CacheRead)
	if err != nil {
		return Totals{}, fmt.Errorf("store: usage totals: %w", err)
	}
	return t, nil
}

// UsageBySource returns one aggregate row per source, ordered by source name
// for stable rendering.
func (s *Store) UsageBySource() ([]SourceTotals, error) {
	rows, err := s.db.Query(`
		SELECT source, COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_creation), 0), COALESCE(SUM(cache_read), 0)
		FROM token_usage
		GROUP BY source
		ORDER BY source`)
	if err != nil {
		return nil, fmt.Errorf("store: usage by source: %w", err)
	}
	defer rows.Close()

	var out []SourceTotals
	for rows.Next() {
		var st SourceTotals
		if err := rows.Scan(&st.Source, &st.InputTokens, &st.OutputTokens,
			&st.CacheCreation, &st.CacheRead); err != nil {
			return nil, fmt.Errorf("store: scan usage: %w", err)
		}
		out = append(out, st)
	}
	return out, rows.Err()
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/store/ -run TestRecordUsage -v && go test ./internal/store/ -run TestUsageTotalsEmpty -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/store/store.go internal/store/usage.go internal/store/usage_test.go
git commit -m "feat(store): token_usage table + RecordUsage/UsageTotals/UsageBySource"
```

---

### Task 2: Parser — capture output tokens + raw fields

**Files:**
- Modify: `internal/claude/event.go` (`ResultEvent`, ~line 10-23)
- Modify: `internal/claude/parser.go` (`rawMessage.Usage` ~line 24, `result` case ~line 80-82)
- Modify: `internal/claude/parser_test.go`

- [ ] **Step 1: Add raw fields to ResultEvent**

In `internal/claude/event.go`, inside the `ResultEvent` struct, after the `ContextTokens int` field (keep the existing comment for `ContextTokens`), add:

```go
	// Raw per-turn token counts from the result event's usage block, kept
	// separate from ContextTokens (which sums the input-side fields for the
	// rotator). These feed the token tracker. OutputTokens is not part of
	// ContextTokens — it is the generation cost, recorded for accounting only.
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
```

- [ ] **Step 2: Write the failing test**

In `internal/claude/parser_test.go`, add:

```go
func TestParseStreamResultUsageFields(t *testing.T) {
	line := `{"type":"result","subtype":"success","usage":{"input_tokens":7,"output_tokens":42,"cache_creation_input_tokens":100,"cache_read_input_tokens":2000}}` + "\n"

	events := make(chan Event, 4)
	if err := ParseStream(context.Background(), strings.NewReader(line), events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)

	var got ResultEvent
	var found bool
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got, found = r, true
		}
	}
	if !found {
		t.Fatal("no ResultEvent emitted")
	}
	if got.InputTokens != 7 || got.OutputTokens != 42 {
		t.Errorf("in/out = %d/%d, want 7/42", got.InputTokens, got.OutputTokens)
	}
	if got.CacheCreationTokens != 100 || got.CacheReadTokens != 2000 {
		t.Errorf("cache = %d/%d, want 100/2000", got.CacheCreationTokens, got.CacheReadTokens)
	}
	// Regression: ContextTokens still sums the three input-side fields.
	if got.ContextTokens != 7+100+2000 {
		t.Errorf("ContextTokens = %d, want %d", got.ContextTokens, 7+100+2000)
	}
}
```

If `context` / `strings` are not already imported in the test file, add them to its import block.

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/claude/ -run TestParseStreamResultUsageFields -v`
Expected: FAIL — `got.OutputTokens` is 0 / field unknown.

- [ ] **Step 4: Implement parsing**

In `internal/claude/parser.go`, add `OutputTokens` to the `Usage` struct (after `InputTokens`):

```go
	Usage struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
```

In the `case "result":` block, replace the `ev := ResultEvent{...}` line so it populates the new fields (keep the `ctxTokens` computation unchanged):

```go
		case "result":
			ctxTokens := raw.Usage.InputTokens + raw.Usage.CacheCreationInputTokens + raw.Usage.CacheReadInputTokens
			ev := ResultEvent{
				Subtype:             raw.Subtype,
				Error:               raw.Error,
				ContextTokens:       ctxTokens,
				InputTokens:         raw.Usage.InputTokens,
				OutputTokens:        raw.Usage.OutputTokens,
				CacheCreationTokens: raw.Usage.CacheCreationInputTokens,
				CacheReadTokens:     raw.Usage.CacheReadInputTokens,
			}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/claude/ -run TestParseStreamResultUsageFields -v && go test ./internal/claude/`
Expected: PASS (full package run guards the rotator-facing `ContextTokens` behaviour).

- [ ] **Step 6: Commit**

```bash
git add internal/claude/event.go internal/claude/parser.go internal/claude/parser_test.go
git commit -m "feat(claude): surface output + raw token fields on ResultEvent"
```

---

### Task 3: `Source` tag + shared recorder + wire main/bus

**Files:**
- Modify: `internal/claude/runner.go` (`RunArgs`, ~line 54-78)
- Create: `cmd/otto/usage.go`
- Modify: `cmd/otto/handler.go` (`runAndReply` result capture ~line 533-562; main call site ~line 488)
- Modify: `cmd/otto/bus.go` (call site ~line 184)

- [ ] **Step 1: Add the Source field to RunArgs**

In `internal/claude/runner.go`, inside `RunArgs`, add (placement near the other metadata fields, e.g. after `Effort`):

```go
	// Source labels which Otto subsystem made this call (e.g. "main", "bus",
	// "toto", "toot") for the token tracker. Metadata only — buildCmdArgs
	// ignores it, so the spawned command is unaffected.
	Source string
```

- [ ] **Step 2: Add the recordUsage helper**

Create `cmd/otto/usage.go`:

```go
//go:build unix

package main

import (
	"log"

	"otto/internal/claude"
	"otto/internal/store"
)

// recordUsage writes one token_usage row for an observed result event. It is
// best-effort: a nil store (tests / store-disabled configs) or a write error
// is logged and swallowed so a usage failure can never break a reply.
func recordUsage(s *store.Store, source, model string, r claude.ResultEvent) {
	if s == nil {
		return
	}
	if model == "" {
		model = "default"
	}
	if err := s.RecordUsage(source, model, r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens); err != nil {
		log.Printf("usage record (%s): %v", source, err)
	}
}
```

- [ ] **Step 3: Record in runAndReply**

In `cmd/otto/handler.go`, inside `runAndReply`, in the `if gotResult {` block (the one that calls `h.otto.setInputTokens(...)` around line 551), add a recordUsage call right after `setInputTokens`:

```go
	if gotResult {
		h.otto.setInputTokens(lastResult.ContextTokens)
		recordUsage(h.store, args.Source, args.Model, lastResult)
```

(Leave the rest of the block — the over-budget warning — unchanged.)

- [ ] **Step 4: Set Source at the main and bus call sites**

In `cmd/otto/handler.go` at the main call site (~line 488), add `Source: "main",` to the `claude.RunArgs{...}` literal:

```go
	h.runAndReply(callCtx, ctx, u.ChatID, claude.RunArgs{
		Prompt:             u.Text,
		Source:             "main",
		SessionID:          h.session.ID(),
		ImagePaths:         imagePaths,
		Model:              model,
		AppendSystemPrompt: composePromptWithTimeAndMemory(h.baseSystemPrompt, h.mem),
	}, h.runner)
```

In `cmd/otto/bus.go` at the call site (~line 184), add `Source: "bus",`:

```go
	h.runAndReply(callCtx, ctx, u.ChatID, claude.RunArgs{
		Prompt:             u.Text,
		Source:             "bus",
		SessionID:          h.session.ID(),
		Model:              model,
		AppendSystemPrompt: composePromptWithTimeAndMemory(h.baseSystemPrompt+extraPrompt, h.mem),
	}, scopedRunner)
```

- [ ] **Step 5: Build to verify it compiles**

Run: `go build ./...`
Expected: success, no errors.

- [ ] **Step 6: Commit**

```bash
git add internal/claude/runner.go cmd/otto/usage.go cmd/otto/handler.go cmd/otto/bus.go
git commit -m "feat(otto): record token usage for main + bus turns"
```

---

### Task 4: Record usage in Toto and Toot event loops

**Files:**
- Modify: `cmd/otto/toto.go` (event loop ~line 276-286; call site ~line 302)
- Modify: `cmd/otto/toot.go` (two event loops ~line 279-287 and ~line 425-437; call sites ~line 295 and ~line 439)

- [ ] **Step 1: Toto — capture the result event**

In `cmd/otto/toto.go`, the event-consumer goroutine currently handles only
`AssistantTextEvent` and `SessionEvent`. Add a local `ResultEvent` capture.
Before the goroutine, add a declaration next to `assistantText` / `capturedSessionID`:

```go
	var lastResult claude.ResultEvent
	var gotResult bool
```

Then add a case to the `switch e := ev.(type)` inside the loop:

```go
			case claude.ResultEvent:
				lastResult = e
				gotResult = true
```

- [ ] **Step 2: Toto — set Source and record after the run**

In the `claude.RunArgs{...}` literal at the Toto call site, add `Source: "toto",`.
Then, after `close(events)` / `<-doneParsing` and before the session-save block, add:

```go
	if gotResult {
		recordUsage(t.store, "toto", totoModel, lastResult)
	}
```

- [ ] **Step 3: Toot — repeat for both loops**

In `cmd/otto/toot.go`, apply the same three changes (declare `lastResult`/`gotResult`,
add the `claude.ResultEvent` case, set `Source: "toot",`, and record after the run
with `recordUsage(t.store, "toot", tootModel, lastResult)`) to **both** event loops
— the reply path (~line 279) and the announce path (~line 425). Each loop has its
own `events`/`doneParsing`, so each gets its own `lastResult`/`gotResult` pair.

For the reply path call site (~line 295):

```go
	err := runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		Source:             "toot",
		SessionID:          t.session.ID(),
		Model:              tootModel,
		Effort:             tootEffort,
		AllowedTools:       tootAllowedTools,
```

For the announce path call site (~line 439):

```go
	err := t.runner.Run(ctx, claude.RunArgs{
		Prompt:             prompt,
		Source:             "toot",
		SessionID:          t.session.ID(),
		Model:              tootModel,
		Effort:             tootEffort,
		DisallowedTools:    []string{"*"},
```

Record after each run (after the respective `<-doneParsing`):

```go
	if gotResult {
		recordUsage(t.store, "toot", tootModel, lastResult)
	}
```

- [ ] **Step 4: Build and run existing pet tests**

Run: `go build ./... && go test ./cmd/otto/ -run 'Toto|Toot' -v`
Expected: build succeeds; existing Toto/Toot tests still PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/toto.go cmd/otto/toot.go
git commit -m "feat(otto): record token usage for Toto + Toot turns"
```

---

### Task 5: Classifier — JSON output + usage capture

**Files:**
- Modify: `cmd/otto/classify.go` (`execClassifier` struct ~line 97-100; `classify` ~line 105-135)
- Modify: wherever `execClassifier{}` is constructed (search below)
- Create: `cmd/otto/classify_test.go` additions (or new test func)

- [ ] **Step 1: Write the failing test for JSON-envelope parsing**

The verdict + usage extraction must be a pure, testable function. Create (or add to)
`cmd/otto/classify_test.go`:

```go
//go:build unix

package main

import "testing"

func TestParseClassifyJSON(t *testing.T) {
	out := `{"type":"result","subtype":"success","result":"CODE","usage":{"input_tokens":12,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":900}}`
	verdict, u, ok := parseClassifyJSON([]byte(out))
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if verdict != "CODE" {
		t.Errorf("verdict = %q, want CODE", verdict)
	}
	if u.InputTokens != 12 || u.OutputTokens != 3 || u.CacheReadTokens != 900 {
		t.Errorf("usage = %+v, want in12 out3 cr900", u)
	}
}

func TestParseClassifyJSONMalformed(t *testing.T) {
	if _, _, ok := parseClassifyJSON([]byte("not json")); ok {
		t.Error("ok = true on malformed input, want false")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/otto/ -run TestParseClassifyJSON -v`
Expected: FAIL — `parseClassifyJSON undefined`.

- [ ] **Step 3: Implement parseClassifyJSON and a usage struct**

In `cmd/otto/classify.go`, add an import for `encoding/json`, and add a small
usage type plus the parser. Reuse `claude.ResultEvent`'s field names for the
output struct so `recordUsage` can consume it directly:

```go
// classifyUsage is the token accounting parsed out of the classifier's JSON
// envelope. Mapped onto a claude.ResultEvent so recordUsage can store it.
type classifyUsage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// parseClassifyJSON extracts the verdict text and token usage from the single
// JSON object `claude -p --output-format json` returns. Returns ok=false on any
// JSON error so the caller can fall back to the default model.
func parseClassifyJSON(b []byte) (verdict string, u classifyUsage, ok bool) {
	var env struct {
		Result string `json:"result"`
		Usage  struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return "", classifyUsage{}, false
	}
	u = classifyUsage{
		InputTokens:         env.Usage.InputTokens,
		OutputTokens:        env.Usage.OutputTokens,
		CacheCreationTokens: env.Usage.CacheCreationInputTokens,
		CacheReadTokens:     env.Usage.CacheReadInputTokens,
	}
	return env.Result, u, true
}
```

- [ ] **Step 4: Run the parse test to verify it passes**

Run: `go test ./cmd/otto/ -run TestParseClassifyJSON -v`
Expected: PASS.

- [ ] **Step 5: Switch the subprocess to JSON output and record usage**

Give `execClassifier` a store field:

```go
type execClassifier struct {
	binary  string
	workDir string
	store   *store.Store // optional; nil disables usage recording
}
```

Add `"otto/internal/store"` and `"otto/internal/claude"` to the imports.

In `classify`, change the output format flag and route the output through
`parseClassifyJSON`:

```go
	cmd := exec.CommandContext(cctx, c.binary,
		"-p", classifyPrompt(message),
		"--model", classifierModel,
		"--no-session-persistence",
		"--dangerously-skip-permissions",
		"--disallowedTools", "*",
		"--output-format", "json",
	)
	if c.workDir != "" {
		cmd.Dir = c.workDir
	}
	cmd.Env = append(os.Environ(), "OTTO_RUNNING=1")

	out, err := cmd.Output()
	if err != nil {
		log.Printf("model router failed (%v) — defaulting to %s", err, ottoDefaultModel)
		return ottoDefaultModel
	}
	verdict, usage, ok := parseClassifyJSON(out)
	if !ok {
		log.Printf("model router: unparseable JSON — defaulting to %s", ottoDefaultModel)
		return ottoDefaultModel
	}
	recordUsage(c.store, "classify", classifierModel, claude.ResultEvent{
		InputTokens:         usage.InputTokens,
		OutputTokens:        usage.OutputTokens,
		CacheCreationTokens: usage.CacheCreationTokens,
		CacheReadTokens:     usage.CacheReadTokens,
	})
	model := parseModelFromVerdict(verdict)
	log.Printf("model router: %q → %s", truncate(message, 60), modelLabel(model))
	return model
```

- [ ] **Step 6: Wire the store into the classifier constructor**

Run: `grep -rn "execClassifier{" cmd/otto/*.go | grep -v _test`
Expected: one construction site (in the bot/handler wiring). Add `store: <theStore>,`
to that literal, passing the same `*store.Store` the handler uses (`h.store` /
whatever local variable holds it at construction). If the construction site has no
store in scope, thread the existing store value through to it — do **not** introduce
a second `store.Open`.

- [ ] **Step 7: Build and run classifier tests**

Run: `go build ./... && go test ./cmd/otto/ -run 'Classify|Verdict' -v`
Expected: build succeeds; tests PASS. (Existing `parseModelFromVerdict` tests are
unaffected — that function still takes a plain string.)

- [ ] **Step 8: Commit**

```bash
git add cmd/otto/classify.go cmd/otto/classify_test.go
git commit -m "feat(otto): classifier JSON output + token usage capture"
```

---

### Task 6: `/tokens` command

**Files:**
- Modify: `cmd/otto/usage.go` (add `formatUsage`)
- Create: `cmd/otto/usage_test.go`
- Modify: `cmd/otto/commands.go` (add `case "/tokens":` ~line 74-81)

- [ ] **Step 1: Write the failing test for formatUsage**

Create `cmd/otto/usage_test.go`:

```go
//go:build unix

package main

import (
	"strings"
	"testing"

	"otto/internal/store"
)

func TestFormatUsage(t *testing.T) {
	totals := store.Totals{InputTokens: 1284302, OutputTokens: 96540}
	bySrc := []store.SourceTotals{
		{Source: "main", Totals: store.Totals{InputTokens: 980120, OutputTokens: 71200}},
		{Source: "classify", Totals: store.Totals{InputTokens: 26482, OutputTokens: 1200}},
	}
	got := formatUsage(totals, bySrc)

	for _, want := range []string{"1,284,302 in", "96,540 out", "main", "classify", "980,120 in"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatUsage output missing %q\n---\n%s", want, got)
		}
	}
}

func TestFormatUsageEmpty(t *testing.T) {
	got := formatUsage(store.Totals{}, nil)
	if !strings.Contains(got, "No token usage recorded yet") {
		t.Errorf("empty output = %q", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/otto/ -run TestFormatUsage -v`
Expected: FAIL — `formatUsage undefined`.

- [ ] **Step 3: Implement formatUsage and a thousands helper**

Add to `cmd/otto/usage.go` (and add `"fmt"` / `"strings"` to its imports):

```go
// formatUsage renders the /tokens reply: a grand total followed by a
// per-source breakdown (input + output only; cache columns stay in the DB).
func formatUsage(totals store.Totals, bySrc []store.SourceTotals) string {
	if len(bySrc) == 0 {
		return "📊 No token usage recorded yet."
	}
	var b strings.Builder
	b.WriteString("📊 Token usage (all-time)\n")
	fmt.Fprintf(&b, "Total: %s in · %s out\n\n",
		thousands(totals.InputTokens), thousands(totals.OutputTokens))
	for _, s := range bySrc {
		fmt.Fprintf(&b, "%-9s %s in · %s out\n",
			s.Source, thousands(s.InputTokens), thousands(s.OutputTokens))
	}
	return strings.TrimRight(b.String(), "\n")
}

// thousands renders n with comma separators (e.g. 1284302 -> "1,284,302").
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./cmd/otto/ -run TestFormatUsage -v`
Expected: PASS.

- [ ] **Step 5: Add the /tokens command**

In `cmd/otto/commands.go`, add a new case to the `switch parts[0]` block (e.g.
after the `/whoami` case):

```go
	case "/tokens":
		if h.store == nil {
			return commandResult{reply: "📊 Token tracking is disabled (no store configured).", handled: true}
		}
		totals, err := h.store.UsageTotals()
		if err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ token totals failed: %v", err), handled: true}
		}
		bySrc, err := h.store.UsageBySource()
		if err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ token breakdown failed: %v", err), handled: true}
		}
		return commandResult{reply: formatUsage(totals, bySrc), handled: true}
```

- [ ] **Step 6: Build and run the full cmd/otto test suite**

Run: `go build ./... && go test ./cmd/otto/`
Expected: build succeeds; all tests PASS.

- [ ] **Step 7: Commit**

```bash
git add cmd/otto/usage.go cmd/otto/usage_test.go cmd/otto/commands.go
git commit -m "feat(otto): /tokens command with per-source breakdown"
```

---

### Task 7: Full verification + docs

**Files:**
- Modify: `README.md` (commands list — add `/tokens`)

- [ ] **Step 1: Run the whole suite**

Run: `go build ./... && go test ./...`
Expected: all packages PASS.

- [ ] **Step 2: Document the command**

In `README.md`, find the list of Telegram commands (near `/whoami` / `/status`)
and add a line:

```
- Send `/tokens` — prints all-time token usage with a per-source breakdown (main / bus / toto / toot / classify).
```

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: document /tokens command"
```

---

## Self-Review Notes

- **Spec §1 (data model):** Task 1. ✓
- **Spec §2 (parser breakdown):** Task 2. ✓ `ContextTokens` regression guarded.
- **Spec §3 (Source attribution + recorder):** Tasks 3–4 (main/bus/toto/toot). ✓
- **Spec §4 (classifier JSON):** Task 5. ✓ Falls back to default model on parse failure.
- **Spec §5 (/tokens command):** Task 6. ✓
- **Spec §6 (testing):** store round-trip (T1), parser fields (T2), classifier JSON (T5), /tokens formatting (T6). ✓
- **Type consistency:** `recordUsage(s *store.Store, source, model string, r claude.ResultEvent)` used identically in Tasks 3/4/5; `claude.ResultEvent` field names (`InputTokens`/`OutputTokens`/`CacheCreationTokens`/`CacheReadTokens`) match across event.go, usage.go, and classify.go; `store.Totals`/`SourceTotals` shapes match between usage.go and usage_test.go. ✓
- **Open item resolved during implementation:** Task 5 Step 6 requires locating the single `execClassifier{}` construction site and threading the existing store in — flagged explicitly rather than guessed.
