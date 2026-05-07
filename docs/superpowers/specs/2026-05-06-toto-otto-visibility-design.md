# Toto/Otto visibility — fix two bugs that make Toto report "idle" while Otto is busy

## Problem

Two screenshots, same minute, same user-visible failure: Toto says "idle.
nothing." (and later "same thing. idle.") while Otto is in fact working on
the user's prior message. Toto is supposed to know what Otto is doing.

The two failures look identical to a user but have different root causes.

### Bug A — batch dispatch race

Trace from the daemon log:

```
13:46:24 polling error: ... read: connection reset by peer (retry in 1s)
13:47:45 pet routing: toto ← "hey toto what's otto doing?"
13:48:19 otto busy → toto (silence=29s) msg="what's he doing now?"
         inflight="can you summarize all my emails and tell me what's going on"
```

The user sent two messages in quick succession ("summarize emails" and
"hey toto what's otto doing?"). Network jitter (the connection-reset
errors right before) plausibly reordered them in Telegram's delivery, so
the polling loop got the batch as `[hey toto, summarize]`.

`runPollingLoop` dispatches each update in a goroutine with a 150 ms
spacing between them — the existing race fix from commit 98b8e7c. That
fix assumes the Otto-bound message lands first in the batch: by the time
the toto-addressed message dispatches 150 ms later, Otto's slot is
claimed and Toto's snapshot reads `Busy: true`.

When the order is reversed, the assumption fails. Toto dispatches first,
fetches `ottoStatus` while Otto is still idle, and reports
"idle. nothing." 150 ms later Otto's dispatch claims the slot — too
late.

### Bug B — Toto session memory overrides new system prompt

Same conversation, one message later. The user typed `what's he doing
now?` (not pet-addressed, so it routed via the dispatch path, hit
`tryAcquireOrSnapshot`, found Otto busy, and went to `toto.BusyReply`
with the full inflight prompt and snippet). The log confirms:
`otto busy → toto (silence=29s) ... inflight="can you summarize..."`.

So Toto received the busy context in his per-call system prompt. He
still answered "same thing. idle."

Toto runs with `--resume <session-id>`, so his conversation history
persists across turns. The previous turn's context said "Otto is IDLE"
(Bug A) and Toto's reply was "idle. nothing." Haiku's next turn weighs
the conversation history (recent assistant message: idle) more heavily
than the silently-changed system prompt (now: busy). The user's "what's
he doing **now**?" reads as "did the answer change?" and Toto echoes the
prior answer.

System-prompt updates between turns of a resumed session are a known
weak point for small models — the model treats prior assistant turns as
ground truth.

## Goals

- Bug A: When two messages arrive in the same Telegram batch and one is
  pet-addressed while the other is Otto-bound, Toto's snapshot must
  reflect the post-Otto-dispatch state regardless of Telegram's
  delivery order.
- Bug B: When Otto's busy/idle state changes between Toto turns, Toto's
  reply must reflect the current state, not echo a stale prior turn.

Out of scope (per user direction):

- Tracking Otto's recent past activity when Otto is currently idle. The
  current design only surfaces Otto's *in-flight* work to Toto; that
  stays.
- Cross-batch races (where Otto-bound and pet messages arrive in
  separate Telegram polls). The existing `dispatchBatchSpacing` race
  fix already does not cover this case, and it is not part of either
  observed bug.

## Design

### Fix A — pet-last batch reorder

Change site: `cmd/otto/handler.go`, `runPollingLoop`'s update loop.

Before dispatching, partition the batch into two ordered slices:

- `nonPet` — updates that do NOT match a pet name (commands, photos,
  Otto-bound text)
- `pet` — updates that DO match a pet name (Toto, Toot, future pets)

Dispatch `nonPet` first, then `pet`, preserving original order within
each group. Keep the existing 150 ms inter-dispatch spacing.

Why this works: any sibling Otto-bound message in `nonPet` reaches
`tryAcquireOrSnapshot` before any `pet` dispatch begins. With the 150 ms
spacing between dispatches, Otto's slot is claimed before the pet's
goroutine reads the snapshot. Telegram's delivery order no longer
matters for this race.

Why pet-last is benign even when the user typed pet-first: the only
case where reordering changes anything is when the pet and Otto-bound
messages were sent close enough to land in the same Telegram poll
(typically <1 s apart). The user's send-order intent is not strong at
that resolution. Toto's direct-address path treats Otto status as
silent context anyway — Toto doesn't volunteer "he's busy" unless the
user asks.

Photos: classified as non-pet (matches existing dispatch behavior:
photos always go to Otto regardless of caption text).

Commands: classified as non-pet. They don't acquire the Otto slot, so
their position in the order is irrelevant to correctness; they reply
quickly and move on.

Helper:

```go
func isPetAddressed(u telegram.Update, pets *petRegistry) bool {
    if pets == nil || len(u.PhotoIDs) > 0 {
        return false
    }
    _, _, ok := pets.Match(u.Text)
    return ok
}
```

The classification mirrors the dispatch path's pet check exactly, so
there is no risk of the partition disagreeing with downstream routing.

### Fix B — inject live Otto status into Toto's user-side prompt

Change site: `cmd/otto/toto.go`, `replyWithContext`.

Today, the live status (busy/idle, prompt, snippet) is appended to the
**system prompt** via `--append-system-prompt`. The bug is that
session-resumed conversations weigh prior assistant turns over silently
changed system prompts.

Move the volatile live-status fields (Otto's busy flag, current
prompt, snippet) from the system prompt to a **leading note on the
user-side prompt**. The system prompt keeps:

- The mode-classification header
  (`OTTO IS CURRENTLY WORKING ON THIS FOR THE USER` /
  `THE USER ADDRESSED YOU DIRECTLY`) — tells Toto which voice to use.
- The standing meta-instruction about how to handle the snippet
  ("Use this only to ground your reply… do NOT relay Otto's words
  verbatim") — this is a rule, not data.

What moves to the user-side prefix: the live values themselves — the
"is Otto busy?" boolean, his current prompt, and the snippet tail.
These are the fields that change between turns and that the model must
re-read on every reply.

Concretely, the system prompt for the busy-fallback path becomes:

```
{TOTO.md persona}

OTTO IS CURRENTLY WORKING ON THIS FOR THE USER. The exact prompt and
the tail of his in-progress reply are included as a status note at
the top of the user message. Use them only to ground your reply in
reality — e.g. "he's typing about your gmail right now" if the
snippet is about gmail. Do NOT relay Otto's words to the user
verbatim or pretend his answer is yours.
```

…and the user-side prompt becomes (single block, prepended to
whatever the user typed):

```
(otto status: busy on "can you summarize all my emails...")
(tail of his reply: "...Re-Tension equity vesting agreement...")

what's he doing now?
```

When Otto is idle:

```
(otto status: idle)

hey toto what's up
```

When the snippet is empty (Otto just claimed the slot, no streaming
output yet):

```
(otto status: busy on "can you summarize all my emails...")

what's he doing now?
```

Why this fixes Bug B: each turn's user message includes the live status
inline. The conversation history that Haiku reads on the next turn
contains the prior status note alongside Toto's prior reply — so when
the user asks "what's he doing now?", the model sees "previous turn:
status=idle, my reply=idle. nothing. current turn: status=busy on X"
and naturally updates.

Why this is safe for the cat persona: the parenthetical note format
matches a "stage direction" the model can read without echoing it
verbatim. Haiku is reliable at this kind of structured prefix. The TOTO
persona's "HONESTY" rule ("don't invent, 'he's still at it' is fine")
still applies — the note gives Toto truth to ground his reply in.

Apply in both code paths:

- `BusyReply` (busy fallback): always prepend the user-side note
  using the explicit `ottoPrompt` and `ottoSnippet` passed in.
- `Reply` (direct address): prepend only when `t.ottoStatus != nil`
  (mirrors today's nil-guard so tests that don't wire the status
  function aren't disturbed).

The direct-address path's system prompt undergoes the parallel
transformation: keep the existing `THE USER ADDRESSED YOU DIRECTLY`
header (TOTO.md uses that exact substring to detect mode 2) and the
"Mention this only if the user asks…" meta-instruction, but move the
busy/idle live values to the user-side note.

The existing system-prompt mode markers stay so TOTO.md's "you can tell
which mode by reading the per-call prompt" instruction continues to
work without persona-file edits.

The user message wrapping happens after the empty-prompt fallback for
direct-address pings (`prompt = "(the user pinged you with no
content...)"`) so a bare "toto" still gets a status-note prefix.

## Affected files

- `cmd/otto/handler.go` — partition + reorder in `runPollingLoop`; add
  `isPetAddressed` helper.
- `cmd/otto/toto.go` — build a live-status string from
  busy/prompt/snippet inputs; prepend to user prompt; remove the
  status body from the system prompt (keep mode headers).
- `cmd/otto/handler_test.go` — new test: batch `[pet, otto]` →
  Toto's call sees Otto busy on the sibling prompt.
- `cmd/otto/toto_test.go` — new file or new tests in an existing file:
  verify the live-status prefix appears in `RunArgs.Prompt` for both
  busy-fallback and direct-address paths, with and without snippet,
  and idle direct-address.

No persona-file changes (TOTO.md / `~/.config/otto/toto_persona.md`
stay as-is).

## Testing

Both fixes are deterministic given the existing test fakes:

- `fakeBot` already supports multi-update batches.
- `fakeRunner` records `RunArgs`, including `Prompt` — perfect for
  asserting the user-side status prefix.
- The pet-last reorder is observable through the order of
  `runner.called` entries and the contents of each call's `Prompt`
  (Otto sees its own message; Toto's call sees the status prefix
  referencing Otto's prompt).

Manual verification after merge: send the screenshot scenario again
("summarize" + "hey toto" within a second) and confirm Toto reports
the busy status; then ask "what's he doing now?" and confirm Toto
does not echo "idle".

## Non-goals / explicitly not doing

- No persistent "last completed prompt" memory in `ottoState`.
- No changes to TOTO.md persona text.
- No bumping of `dispatchBatchSpacing`.
- No reset of Toto's session on busy/idle transitions (would lose
  the cat-in-context conversational continuity).
