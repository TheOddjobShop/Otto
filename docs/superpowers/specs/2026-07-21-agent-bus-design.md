# Otto Agent Bus — Design Spec

**Date:** 2026-07-21
**Status:** Retroactive — documents behavior as shipped in v0.7.2
**Author:** written after the fact from the code

## Problem

Otto, Toto, and Toot are three separate Claude Code subprocesses with separate
sessions. Before the bus they could not talk to each other at all: Toto could
only tell the user "otto's busy," never hand the request to Otto; Otto could not
ask Toto for a one-liner; nothing could reach Toot outside a release
announcement. Any cross-persona interaction had to be re-typed by the user.

Two constraints shape the solution:

1. The personas run in **different processes**, spawned per-turn. There is no
   shared address space and no long-lived channel between them.
2. Agents that can message each other can **loop forever** — a cost and
   noise hazard with no human in the loop to stop it.

## Goal

A durable, single-writer-safe message queue in the existing state DB, drained by
a poller inside `cmd/otto`, that routes a message to whichever persona it is
addressed to and gives the recipient enough context to reply back along the
chain — with a hard bound on chain depth.

## Non-goals

- **Multi-user / multi-chat routing.** Every dispatch targets
  `h.allow.UserID()` (`cmd/otto/bus.go:80`); if that is zero the row is dropped.
  Otto is single-user by construction.
- **At-least-once delivery.** `DequeueAll` commits `delivered=1` *before* the
  dispatcher runs (`internal/store/inbox.go:120-207`). A crash between commit
  and dispatch drops that batch permanently. The trade is documented in the
  method comment: loss over duplicates.
- **A visible relay banner.** See "Why no banner" below.
- **Dynamic agent registration.** `validTargets` is a closed set
  (`inbox.go:27`); adding a target requires a matching `switch` arm in
  `dispatchBusMessage`.
- **User-sourced enqueue.** `source = "user"` is accepted and dispatched
  (`bus.go:133-139`), but nothing in-tree produces such a row today. It is a
  supported shape, not a live path.

## The inbox table

Declared in the idempotent `schema` block (`internal/store/store.go:47-57`):

```sql
CREATE TABLE IF NOT EXISTS inbox (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        TEXT    NOT NULL,   -- RFC3339Nano UTC
    target    TEXT    NOT NULL,   -- 'otto' | 'toto' | 'toot'
    source    TEXT    NOT NULL,   -- 'user' | 'agent'
    sender    TEXT    NOT NULL,   -- 'otto'|'toto'|'toot'; '' when source='user'
    body      TEXT    NOT NULL,
    delivered INTEGER NOT NULL DEFAULT 0,
    hop       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS inbox_undelivered ON inbox(delivered, id);
```

`hop` postdates the original table, so `Open` sniffs for it with
`SELECT hop FROM inbox LIMIT 1` and `ALTER TABLE`s it in when missing
(`store.go:86-94`) — the only column migration in the store.

`Enqueue` (`inbox.go:81`) validates before writing: hop in `[0, MaxBusHop]`,
target and source in their closed sets, `sender` empty iff `source == "user"`
and a valid agent name otherwise, body non-empty after trimming. Over-cap hops
return the sentinel `ErrBusHopExceeded` rather than a generic error so callers
can distinguish "chain is over" from "write failed."

## The drain loop

`runBusDrain` (`cmd/otto/bus.go:29`) is a long-lived goroutine started from
`main` alongside the updater, rotator and pruner. It tickers on
`busDrainInterval = 250ms` (`bus.go:20`) — chosen short enough that an
Otto→Toto ping reads as conversational and long enough to coalesce bursts and
idle at ~zero CPU. It is a package var so tests can crank it down.

Each tick calls `store.DequeueAll`, which in one transaction selects up to
`inboxDequeueCap = 64` undelivered rows in id order and marks exactly those rows
delivered. Two details matter:

- The transaction opens with a **no-op write** (`UPDATE inbox SET delivered =
  delivered WHERE 0`, `inbox.go:148`) to force SQLite to take the write lock up
  front. `database/sql` starts transactions deferred; without this the later
  `UPDATE` would have to upgrade a read snapshot and could fail immediately with
  `SQLITE_BUSY_SNAPSHOT`, which `busy_timeout` deliberately does not retry.
- The mark-delivered predicate is an **id range** (`id <= maxID`), not an
  `IN (...)` list, so it cannot hit `SQLITE_LIMIT_VARIABLE_NUMBER` if the cap is
  ever raised.

Every dequeued row is dispatched in its own goroutine registered on
`h.dispatchWG` (`bus.go:55`), so `WaitDispatches()` covers bus-sourced turns as
well as Telegram-path ones — otherwise shutdown could exit while a bus turn was
between `runner.Run` returning and `session.Set`/`logTurn` completing.

Delivered rows are garbage-collected by the hourly pruner, which keeps the most
recent `pruneKeepInbox = 500` delivered rows and never touches undelivered ones
(`cmd/otto/prune.go:26`, `store.PruneInbox`).

## Hop counting and the cap

`store.MaxBusHop = 3` (`inbox.go:21`). Semantics:

- A row that did not originate from an agent chain carries `hop = 0`.
- Every MCP tool that enqueues does so at `hop+1`, where `hop` is the depth of
  the message the calling agent is currently answering
  (`server.go:280`, `server.go:334`).
- `Enqueue` refuses at `hop > MaxBusHop` with `ErrBusHopExceeded`; the tool
  handlers translate that into a model-readable refusal —
  "agent-to-agent conversation reached its 3-hop cap; ending here." — so the
  model stops rather than retrying.

The cap is therefore enforced twice: softly in the prompt ("HOPS REMAINING"),
hard in the store.

## Crossing the process boundary

The dispatcher knows the hop; the code that needs it (`forward_to_otto`,
`message_toto`, `message_toot`) runs inside the `otto-memory` MCP server, which
is a **grandchild process** — `cmd/otto` spawns `claude`, which spawns the MCP
stdio server. A Go `context.Context` cannot cross that boundary. So there are
two transports for the same two values:

| | in-process | cross-process |
|---|---|---|
| hop | `store.WithBusHop(ctx, n)` / `store.BusHopFromCtx` (`inbox.go:59-69`) | `OTTO_BUS_HOP` env var |
| sender | `ctxKeyBusSender` (`cmd/otto-memory/server.go:69`) | `OTTO_BUS_SENDER` env var |

`busEnv(hop, sender)` (`bus.go:214`) builds the env map; each persona wraps its
runner with `runner.WithEnv(...)` before running, which returns a *copy* of the
`execRunner` with merged env rather than mutating the shared one.

On the reading side, `hopFromCtxOrEnv` / `senderFromCtxOrEnv`
(`server.go:42`, `server.go:57`) prefer the ctx value (used by tests and any
in-process path) and fall back to the env var (production). Absent both, hop is
0 and the sender falls back to `defaultSenderFor(target)` — a two-way guess:
`message_*` defaults to `otto`, `forward_to_otto` defaults to `toto`.

Because that guess cannot name a *third* agent, **Toto and Toot stamp the env
unconditionally**, even on a direct Telegram turn (`toto.go:359-370`,
`toot.go:303-311`). Without it, Toto calling `message_toot` on a non-bus turn
would be misattributed to "otto."

The dispatcher also wraps the ctx with `store.WithBusHop` for agent-sourced rows
(`bus.go:77`), which is what the in-process leg of the same mechanism reads.

## The BUS CONTEXT prompt block

`busPromptBlock(bc, selfName)` (`bus.go:226`) renders a fixed-width block
prepended to the recipient's per-call system prompt. It states who sent the
message, who the recipient is, `Hop: N of 3`, and `HOPS REMAINING: N` twice
(once in the header table, once as a bare line the model is more likely to
attend to).

Below the header it renders one of two bodies:

- **remaining > 0** — explains that plain Telegram text is visible to the user
  but does **not** reach the sender, and that `message_<sender>(message, reason)`
  is the only path back. Gives a three-step pattern: compose, call the tool,
  optionally also send matching Telegram text so the user can follow along.
- **remaining == 0** — instructs the model to stop: reply in plain Telegram text
  only, make no further tool calls, and wind down gracefully ("alright, that's
  me out for this thread") rather than getting cut off mid-question.

`selfName` exists so the tool hint reads naturally — the model is told
"call `message_toto`" rather than having to work out which participant isn't
itself.

The three persona files carry the matching instructions so the behavior is
reinforced from the base prompt as well as the per-call block: `SYSTEM.md`
("BUS REPLY (BUS CONTEXT present)"), `TOTO.md` ("BUS HOPS — KNOW WHEN TO STOP"),
`TOOT.md` (same section, in the owl's register).

## Routing

`dispatchBusMessage` (`bus.go:74`) switches on `m.Target`:

- `toto` → `h.toto.BusReply(...)`
- `toot` → `h.findToot().BusReply(...)` — Toot is not a handler field (only Toto
  is, because the busy-handoff fast path needs it), so it is looked up out of
  the pet registry by type (`bus.go:282`)
- `otto` → `dispatchBusToOtto`
- anything else → logged and dropped

A missing pet or an unknown target logs and drops; nothing is surfaced to the
user.

**User-sourced vs agent-sourced** diverge only on the Otto path
(`dispatchBusToOtto`, `bus.go:117`):

- `source == "user"` → straight into `handleMessage`, with **no** BUS CONTEXT and
  no ctx hop wrapping. Such a row is an ordinary Telegram message that took a
  detour through the queue; telling Otto he is mid-chain would be a lie.
- `source == "agent"` → `handleBusOttoMessage` (`bus.go:157`), which builds a
  hop/sender-scoped runner and a BUS CONTEXT suffix and passes both explicitly
  into `handleBusMessage`.

Toto and Toot always receive `busContextFromMsg(m)`; their `BusReply` methods
are only reachable from the bus.

The scoped runner and prompt suffix are passed as **arguments** rather than
temporarily assigned to `h.runner` / `h.baseSystemPrompt`. A save/restore defer
would be correct today (both readers go through the Otto slot), but the
happens-before is implicit and a future reader outside the slot would introduce
a race the detector cannot see (`bus.go:150-156`).

`handleBusMessage` otherwise mirrors the Telegram path: it registers a cancel
func on the Otto slot, starts the watchdog, runs the model router, and calls
`runAndReply` with `Source: "bus"` so token usage is attributed separately.

## Why no visible relay banner

`dispatchBusMessage`'s doc comment states the decision directly: no banner is
sent, so the recipient's own persona reply is the only visible artifact and the
chat reads as a real conversation between characters rather than a relay log.
The `reason` argument every bus tool requires is not dead, though — it is
prefixed onto the message body itself as `(from <sender> — <reason>)`
(`server.go:278`, `server.go:332`), so the *recipient* reads why it was pinged,
and the user sees it only if the recipient paraphrases it.

Operators still get the full chain: `dispatchBusMessage` logs
`bus dispatch: id=… <sender>→<target> hop=N source=…` to the journal
(`bus.go:88`).

## Interaction with the busy handoff

`dispatchBusToOtto` claims the Otto slot with `tryAcquireOrSnapshot`
(`bus.go:123`) — the same atomic claim-or-snapshot the Telegram dispatch path
uses. If Otto is already busy the bus message is **not** queued behind him and
not retried; instead Toto answers with the busy-fallback prompt grounded in
Otto's in-flight prompt and reply-tail snippet (`h.toto.BusyReply`). This is why
`toto` is a first-class handler field. The row has already been marked
delivered, so a bus message that arrives while Otto is busy is answered by Toto
and then gone — consistent with the at-most-once contract above.

## Testing

`cmd/otto/bus_test.go` and `internal/store/inbox_test.go` cover: enqueue
validation (targets, sources, sender/source pairing, empty body, negative and
over-cap hops), dequeue transactionality and the delivered-marking range,
prune bounds, hop propagation through dispatch, the BUS CONTEXT block's
remaining>0 / remaining==0 branches, and the busy-handoff branch of
`dispatchBusToOtto`.

## Affected files

- `internal/store/store.go` — `inbox` schema + `hop` column migration.
- `internal/store/inbox.go` — `Enqueue`, `DequeueAll`, `PruneInbox`,
  `MaxBusHop`, `ErrBusHopExceeded`, `WithBusHop` / `BusHopFromCtx`.
- `cmd/otto/bus.go` — drain loop, dispatch, `busContext`, `busEnv`,
  `busPromptBlock`.
- `cmd/otto/toto.go`, `cmd/otto/toot.go` — `BusReply` + unconditional env stamp.
- `cmd/otto-memory/server.go` — `forward_to_otto`, `message_toto`,
  `message_toot`, env/ctx hop+sender resolution.
- `cmd/otto/prune.go` — delivered-row retention.
- `SYSTEM.md`, `TOTO.md`, `TOOT.md` — persona-level bus instructions.
