# Otto Token Tracker — Design

Date: 2026-06-15
Status: Approved (pending implementation plan)

## Goal

Track how many tokens every Claude invocation Otto makes consumes, across **all**
the subprocesses Otto spawns — the main bot, the agent-bus path, the Toto and
Toot pet personas, and the Haiku model-router classifier. Persist a full per-call
breakdown (input / output / cache-creation / cache-read) to the state DB, attribute
each call to its source, and expose a `/tokens` Telegram command that prints the
running total plus a per-source breakdown.

## Background — how Otto invokes Claude today

There are two kinds of Claude subprocess and several call sites:

1. **Runner-based calls** — all go through `claude.Runner.Run`
   (`internal/claude/runner.go:193`, `exec.CommandContext` with
   `--output-format stream-json`). Parsed by `internal/claude/parser.go`.
   Call sites:
   - `cmd/otto/handler.go:488` — main Telegram turn, via `runAndReply`.
   - `cmd/otto/bus.go:184` — agent-bus turn, via `runAndReply`.
   - `cmd/otto/toto.go:302` — Toto pet, **its own event loop** (not `runAndReply`).
   - `cmd/otto/toot.go:295`, `toot.go:440` — Toot pet, **its own event loops**.
2. **The classifier** — `cmd/otto/classify.go:114`, a separate
   `exec.CommandContext` with `--output-format text`. Bypasses the Runner and
   currently emits **no** usage data.

`parser.go` already reads the result event's `usage` block but only sums the
three input-side fields into `ResultEvent.ContextTokens`, which the session
rotator (`cmd/otto/rotate.go`) uses to decide when to clear a session. Output
tokens are not captured anywhere today, and nothing is persisted or surfaced.

## Design

### 1. Data model — new `token_usage` table

Reuse the existing SQLite store (`internal/store`). One append-only row per
Claude invocation. Added to the idempotent `schema` block in
`internal/store/store.go` — a brand-new table, so `CREATE TABLE IF NOT EXISTS`
is sufficient and no column-migration logic is required.

```sql
CREATE TABLE IF NOT EXISTS token_usage (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    source          TEXT    NOT NULL,  -- 'main' | 'bus' | 'toto' | 'toot' | 'classify'
    model           TEXT    NOT NULL,  -- e.g. claude-opus-4-8, claude-haiku-4-5
    input_tokens    INTEGER NOT NULL,
    output_tokens   INTEGER NOT NULL,
    cache_creation  INTEGER NOT NULL,
    cache_read      INTEGER NOT NULL,
    ts              INTEGER NOT NULL   -- unix seconds
);
CREATE INDEX IF NOT EXISTS token_usage_source ON token_usage(source);
```

New `store.Store` methods (new file `internal/store/usage.go`):

- `RecordUsage(source, model string, in, out, cacheCreate, cacheRead int, ts int64) error`
- `UsageTotals() (Totals, error)` — grand total across all rows.
- `UsageBySource() ([]SourceTotals, error)` — one aggregate row per source,
  ordered by source name for stable output.

Where `Totals` / `SourceTotals` carry input/output/cache-creation/cache-read sums.

### 2. Capturing the full breakdown in the parser

`ResultEvent.ContextTokens` stays exactly as is (the rotator depends on it).
Add the raw fields alongside it (`internal/claude/event.go`):

```go
type ResultEvent struct {
    // ... existing fields (Subtype, Error, PermissionDenials, ContextTokens) ...
    InputTokens         int // new
    OutputTokens        int // new — not captured today
    CacheCreationTokens int // new
    CacheReadTokens     int // new
}
```

In `internal/claude/parser.go`, add `OutputTokens int \`json:"output_tokens"\``
to the `Usage` struct and populate the four new `ResultEvent` fields from it.
`ContextTokens` is still computed as
`InputTokens + CacheCreationInputTokens + CacheReadInputTokens`. Purely additive.

### 3. Attribution — `Source` on `RunArgs`

Add `Source string` to `claude.RunArgs` (`internal/claude/runner.go`). It is
metadata only — `buildCmdArgs` ignores it, so the spawned command is unchanged.
Each call site sets it: `"main"`, `"bus"`, `"toto"`, `"toot"`.

A shared helper on the handler/otto type:

```go
func (h *handler) recordUsage(source, model string, r claude.ResultEvent)
```

writes one row via `store.RecordUsage`, using the current time for `ts`. It is
best-effort: a write error is logged, never surfaced to the user and never
blocks a reply. It is called wherever a `ResultEvent` is observed:

- `runAndReply` (`handler.go`) — reads `args.Source`; covers main + bus.
- Toto's event loop (`toto.go`).
- Toot's two event loops (`toot.go`).

The `model` recorded is the model the call used (`RunArgs.Model`, or the
relevant persona's configured model when `RunArgs.Model` is empty).

### 4. The classifier (special case)

Switch `classify.go` from `--output-format text` to `--output-format json`,
which returns a single JSON object containing both the assistant's text result
and a `usage` block. Parse the verdict text out of the JSON envelope (the
existing `parseModelFromVerdict` logic is unchanged — it just reads from the
JSON field instead of raw stdout) **and** the `usage` block, then record a
`"classify"` row with model `claude-haiku-4-5`.

If the JSON fails to parse, fall back to the existing default-model behaviour
(routing must never block or break a reply) and skip the usage row for that call.

### 5. The `/tokens` command

A read-back command added to the Telegram command router. Prints the running
total and a per-source breakdown, e.g.:

```
📊 Token usage (all-time)
Total: 1,284,302 in · 96,540 out

main     980,120 in · 71,200 out
bus       45,000 in ·  3,100 out
toto     180,400 in · 18,900 out
toot      52,300 in ·  2,140 out
classify  26,482 in ·  1,200 out
```

The on-screen view stays compact (input + output per source); cache-creation and
cache-read are persisted in the DB for later analysis but folded out of the
default command output to keep the Telegram message readable. Numbers are
thousands-separated for legibility.

### 6. Testing

- **Store** (`internal/store/usage_test.go`): round-trip — insert several rows
  across sources, assert `UsageTotals` and `UsageBySource` aggregate correctly.
- **Parser** (`internal/claude/parser_test.go`): a `result` line with all four
  usage fields populates the new `ResultEvent` fields and still computes
  `ContextTokens` correctly (regression guard for the rotator).
- **Classifier** (`cmd/otto/classify_test.go`): a JSON envelope parses to the
  right verdict **and** the right usage numbers; a malformed envelope falls back
  to the default model and records no row.
- **`/tokens` formatting** (`cmd/otto/commands_test.go`): rendering from a known
  set of aggregated rows produces the expected layout.

## Non-goals

- Dollar-cost estimation (per-model pricing) — explicitly out of scope for now.
- Per-reply token footers or log-only surfaces — the chosen surface is persisted
  DB history plus the `/tokens` command.
- Token accounting inside the MCP servers (otto-memory, Ollama embeddings) —
  those are passive tool providers; Claude reports usage at the outer level.

## Affected files

- `internal/store/store.go` — add `token_usage` to schema.
- `internal/store/usage.go` (new) — `RecordUsage`, `UsageTotals`, `UsageBySource`.
- `internal/claude/event.go` — new `ResultEvent` fields.
- `internal/claude/parser.go` — capture `output_tokens`, populate new fields.
- `internal/claude/runner.go` — `Source` field on `RunArgs`.
- `cmd/otto/handler.go` — `recordUsage` helper; call from `runAndReply`; set source.
- `cmd/otto/bus.go` — set `Source: "bus"`.
- `cmd/otto/toto.go` — set `Source: "toto"`; record in event loop.
- `cmd/otto/toot.go` — set `Source: "toot"`; record in both event loops.
- `cmd/otto/classify.go` — JSON output; parse verdict + usage; record `"classify"`.
- `cmd/otto/commands.go` — `/tokens` command.
- Tests as listed in §6.
