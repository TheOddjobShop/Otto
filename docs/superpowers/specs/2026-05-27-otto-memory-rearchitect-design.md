# Otto Memory Rearchitect — Design Spec

**Date:** 2026-05-27
**Status:** Approved (design); implementation plan pending
**Author:** brainstormed with user

## Problem

Otto's conversation state is a single ever-growing Claude Code session transcript,
replayed in full every turn via `--resume`. One design choice causes four pains:

1. **Cost** — every Telegram message replays the entire transcript + reloads all
   MCP tool schemas. Token spend per turn grows without bound.
2. **Forgetting** — the only durable state is that transcript. It leans entirely on
   Claude Code auto-compact; `/new` wipes everything; facts are lost across compacts.
3. **Latency** — each message cold-starts a `claude -p` subprocess and re-inits four
   MCP servers via `npx`.
4. **Cross-persona blindness** — Toto and Toot run separate sessions and cannot see
   Otto's conversation. Toto can only see a 600-byte tail snippet of Otto's in-flight
   reply (`ottoSnapshot`), never the actual history.

## Goal

Otto owns its conversation + memory state. Token cost per turn stays flat as memory
grows. Durable facts survive session resets, compacts, and `/new`. Toto/Toot share the
same memory. Architecture adopted from the **Hermes Agent memory system** (Nous
Research, MIT) — design only; no code imported (Hermes is Python).

## Non-goals (v1)

- External memory providers (Honcho, Mem0, Hindsight, etc.) — YAGNI for single-user.
- Usage-history / predictive rotation — idle timer captures ~95% of the benefit.
- Embedding the curated core — it is tiny and always present; semantic search there is
  pointless.
- Fully stateless per-message context assembly — we keep `--resume` for the rolling
  short-term window and bound it via idle-gated rotation instead.

## Architecture overview

Two-tier memory, Hermes-faithful:

### Tier 1 — Curated core (always injected, bounded)

- `~/.config/otto/memory/USER.md` — identity, role, preferences, communication style.
  Cap ~500 tokens (~1,375 chars).
- `~/.config/otto/memory/MEMORY.md` — environment facts, project conventions, tool
  quirks, lessons learned. Cap ~800 tokens (~2,200 chars).
- Human-readable, hand-editable. Injected into **every** Otto / Toto / Toot call via
  `--append-system-prompt`. ~1,300-token floor, prefix-stable across turns.

### Tier 2 — Episodic + overflow store (on-demand, unbounded-safe)

- `~/.config/otto/state.db` — SQLite, owned by Otto. Contains:
  - `turns` — append-only log of every exchange `(id, persona, role, content, ts)`
    with an FTS5 virtual table for keyword search.
  - `memories` — overflow durable facts that didn't fit the bounded core, plus
    embedded turn chunks. Columns include `content`, `embedding` (BLOB),
    `embedding_model` (dimension/version tag), `kind`, `ts`.
- Queried only on demand (semantic top-k + FTS5), **never bulk-injected**, so it grows
  freely while per-turn token cost stays flat. This is the answer to "memory files get
  big": the core is bounded by hard cap; everything else is retrieved by relevance.

## Components

### 1. `otto-memory` MCP server (new Go binary, shipped in this repo)

A small MCP server Otto adds to `mcp.json`. Tools:

- `memory_add(target, content)` — append a fact. `target` = `user` | `memory`. Writes
  to the corresponding `.md` file.
- `memory_replace(target, old_text, content)` — substring match (Hermes-style);
  ambiguous match returns an error.
- `memory_remove(target, old_text)` — substring delete.
- `session_search(query)` — retrieval over Tier 2. Runs **semantic top-k** (via the
  Embedder chain) and **FTS5 keyword** in parallel, merges results. FTS5 catches exact
  tokens (error codes, identifiers) that vector search misses.

Write-path rules (apply to every `add`/`replace`):

- **Security scan** — reject prompt-injection patterns, credential/secret exfiltration,
  SSH-backdoor shapes, and invisible/zero-width Unicode. Reject exact duplicates.
- **Capacity** — when the target `.md` file exceeds 80% of its cap, `add` errors with
  the current contents, signaling the model to consolidate (merge related entries, drop
  stale facts) via `replace` before retrying. Hard fail at 100%.

Access control:

- **Otto** — full memory toolset (read core via injection; write via tools; search).
- **Toto / Toot** — read-only: `session_search` only. Their personas already deny all
  other tools (`--disallowedTools "*"`); the memory core is injected into their prompts
  too, so they share Otto's facts and recent-conversation recall.

### 2. Embedder chain (in `otto-memory`)

One `Embedder` interface; an ordered, health-checked chain. First healthy backend wins.
Keyword is the terminal floor so memory never hard-fails.

```
1. Ollama · embeddinggemma     (primary — best quality, ~300M)
2. Ollama · nomic-embed-text   (fallback — lighter, ~137M)
3. FTS5 keyword (no embeddings) (terminal floor — always local, zero deps)
```

- Otto calls Ollama at `http://localhost:11434/api/embeddings`.
- Vectors stored as SQLite BLOBs; brute-force cosine in Go (single-user scale = at most
  a few thousand rows → sub-millisecond search; no vector DB needed).
- Every stored vector is tagged with `embedding_model` (name + dimension). On a backend
  swap, a dimension/model mismatch triggers lazy re-embedding rather than corrupt
  comparisons.
- If no embed backend is healthy, `session_search` degrades to FTS5-only and logs the
  degradation. Semantic recall resumes automatically when a backend returns.

`setup.sh` installs Ollama and pulls `embeddinggemma` (with `nomic-embed-text` as a
secondary pull) on Linux; documents the manual step on macOS.

### 3. Memory injector (`cmd/otto`)

Reads `USER.md` + `MEMORY.md` at the start of each turn and prepends the combined core
to the system prompt the three runners already build (persona + operational footer).
Applies to Otto, Toto, and Toot.

### 4. Turn logger (`cmd/otto` + `internal/claude`)

After each exchange (Otto, Toto, or Toot), append `(persona, role, content, ts)` rows
to `state.db.turns`. New episodic content is embedded and indexed (FTS5 + vector) so
`session_search` can find it later. Embedding happens off the reply path so it does not
add user-visible latency.

### 5. Session rotator (`internal/claude` + `cmd/otto`)

**Token tracking.** Extend the stream-json parser to capture `input_tokens` (and
`output_tokens`) from the `result` event. Because `--resume` replays the transcript,
the latest turn's `input_tokens` approximates current session size. Store the latest
value per session.

**Thresholds (configurable):**

> **AMENDED 2026-07-21 — the shipped rule differs from the one designed here.**
> The original three-threshold rule below was replaced during implementation; the
> amendment immediately after it describes what `cmd/otto/rotate.go` actually does.
> Kept side by side because the *why* of the change is the useful part.

Originally designed:

```
soft = 50% of model context window  → eligible to rotate, but wait for idle
hard = 85% of model context window  → rotate now regardless (safety net)
idle = 15 min (configurable; 5–30)  → no user message for this long
```

**Rotate when:** `(tokens >= soft AND idle >= idleWindow)` OR `(tokens >= hard)`.

**As shipped (v0.7.2):**

```
idle = 15 min (rotate_idle_minutes)  → clear regardless of session size
hard = 85% of context (rotate_hard_pct) AND idle >= 5 min (hardRotateActiveGrace)
```

**Rotate when:** `idle >= idleWindow` OR `(tokens >= hard AND idle >= 5min)`.

Two changes, both made in response to observed behavior:

- **`rotate_soft_pct` was dropped entirely** — it does not exist as a config key or a
  field on `rotateConfig`. The idle reset fires on *any* non-empty session rather than
  only ones past 50%. Gating the idle reset on size meant small-but-stale sessions
  survived indefinitely, which is exactly the "answers from stale context" failure the
  rotation was built to prevent. Since continuity comes from the always-injected core
  plus `session_search`, clearing a small idle session costs nothing.
- **The hard cap gained a 5-minute active grace** (commit `fdbde0f`). "Rotate now
  regardless" wiped context mid-conversation: one data-heavy turn (a full Notion backlog
  dump) pushes past 85%, and the user's very next message — seconds later — lands in a
  fresh session with the just-fetched context gone. The cap now waits for the user to
  pause, so it never fires mid-thread.

The normal path remains the idle-gated one. The hard cap is a rare safety net for a
single very long unbroken session.

**Rotation sequence:**

1. **Flush** — one cheap haiku extraction pass distills the closing session into durable
   `memory_add` calls (catches anything the inline hybrid-writes missed).
   *Deferred as a v1 non-goal by the rotation plan; **shipped 2026-07-21** — see
   `cmd/otto/flush.go`. Gated on `rotate_flush` (default on), skipped for sessions under
   5000 tokens, restricted to `memory_add` only (never replace/remove), and bounded at
   90s. Failure is logged and the rotation proceeds.*
2. **Handoff note** — generate a short "open thread" summary of what is currently
   in-flight, persisted for the next session's first turn.
   *Still not implemented, and still a deliberate non-goal: rotation is idle-gated at
   ≥15 minutes, so the thread is almost always already concluded. `session_search`
   recovers older context on demand.*
3. **Clear session** — `Session.Clear()`; the next turn starts a fresh Claude session.
4. **Reseed** — the fresh turn injects: persona + memory core (USER.md + MEMORY.md) +
   semantic-retrieved memories for the incoming message. *(No handoff note — see 2.)*

**Concurrency.** The rotator mirrors the existing watchdog pattern: a ticker goroutine
inspecting `ottoState` (must be `!busy`) + time-since-last-user-message + tracked session
tokens. Rotation acquires the Otto slot (`tryAcquire`) so it cannot race a live turn;
it releases when done.

## Memory write policy (hybrid)

Durable facts are written by:

- **Inline markers / tool calls during Otto's normal reply** — no extra model call. Otto
  is prompted (via the injected core preamble) to call `memory_add`/`memory_replace`
  when it learns a durable fact (a correction, a discovered preference, an environment
  fact, a project convention, a workaround, a completed multi-step workflow).
- **Explicit** — the user says "remember X."
- **Rotation flush** — the closing-session haiku pass (see rotator step 1).

Save proactively on: corrections, discovered preferences, environment facts, project
conventions, tool quirks, completed complex workflows. Skip: trivia, easily re-discovered
info, raw data dumps, session-specific ephemera, anything already in the core. Entries
must be dense and declarative ("User prefers light mode in VS Code, dark in terminal"),
not logs ("On 2026-01-05 the user asked...").

## Data flow (one Otto turn)

```
Telegram msg → allowlist → dispatch
  → assemble system prompt: persona + core(USER.md+MEMORY.md) + current-time + footer
  → claude -p --resume <sid> --append-system-prompt <assembled> --mcp-config <incl otto-memory>
      ↳ during the turn, Claude may call memory_add/replace/remove, session_search
        (retrieval is ON DEMAND via the session_search tool — there is no
         automatic top-k pre-injection per message; see the amendment below)
  → parse stream: assistant text, session id, result(input_tokens, denials)
  → send reply to Telegram (markdown-stripped)
  → log turn to state.db (embed + index, off the reply path)
  → update tracked session tokens

[background ticker] if idle >= idleWindow                     → flush, clear
                    if tokens >= hard AND idle >= 5min         → flush, clear
```

## Config additions (`config.toml`)

- `memory_dir` — default `~/.config/otto/memory/` (holds USER.md, MEMORY.md).
- `state_db_path` — default `~/.config/otto/state.db`.
- `embed_ollama_url` — default `http://localhost:11434`.
- `embed_models` — ordered list, default `["embeddinggemma", "nomic-embed-text"]`.
- ~~`rotate_soft_pct` — default `0.50`.~~ **Not implemented** — dropped during
  implementation; see the rotator amendment above.
- `rotate_hard_pct` — default `0.85`.
- `rotate_idle_minutes` — default `15`.
- `rotate_flush` — default `true`. **Added 2026-07-21.** Enables the step-1 flush
  pass. Absent means on; set `false` to disable.
- `model_context_tokens` — default `200000` (the model's window, for pct math).

All optional with the defaults above so existing `config.toml` files keep working.

## Error handling & resilience

- **Ollama down** → embedder chain degrades to FTS5; logged; auto-recovers.
- **state.db locked/corrupt** → memory tools error to the model (it copes in-prompt);
  Otto's reply path still works (memory is additive, not on the critical path).
- **Rotation flush fails** → log, skip flush, still clear (durable facts from inline
  writes are already saved; worst case one session's un-distilled tail is lost to
  `session_search` but its raw turns remain in `state.db`).
- **Memory core file missing** → treat as empty; do not fail startup (mirrors current
  persona-file handling).
- **Embedding dimension mismatch on swap** → lazy re-embed, never compare across dims.

## Security

- `--dangerously-skip-permissions` stays (unchanged threat model: single-user allowlist
  gate upstream).
- Memory write security scan (above) is new and blocks injection/secret/backdoor/invisible
  -Unicode content before it can be persisted into an always-injected prompt surface —
  this is important because the core is injected verbatim every turn.
- `state.db` and memory files written `0600`, consistent with existing config perms.

## Testing

- `otto-memory` server: unit tests for add/replace/remove substring semantics, dup
  rejection, capacity error at 80%, security-scan rejections, FTS5 + cosine retrieval.
- Embedder chain: fake backends to assert ordering, health-check fallthrough, FTS5
  floor, dimension-mismatch re-embed.
- Rotator: table-driven token/idle threshold logic; flush+handoff+clear sequence with a
  fake runner; concurrency test that rotation acquires the Otto slot and cannot race a
  live turn.
- Injector: core prepended to all three personas.
- Integration: extend `testdata/fake-claude.sh` to emit `result` events with
  `input_tokens` so rotation triggers can be exercised end-to-end via `-tty` mode.

## Implementation phasing (for the plan)

1. SQLite `state.db` + schema + turn logger (no behavior change yet).
2. `otto-memory` MCP server: memory tools + FTS5 `session_search` (keyword only).
3. Memory core files + injector wired into all three runners.
4. Embedder chain + semantic retrieval added to `session_search`.
5. Token capture in parser + session rotator (idle-gated + hard cap, flush, handoff).
6. `setup.sh` (Ollama install/pull) + config additions + docs.

Each phase is independently testable and leaves Otto runnable.
