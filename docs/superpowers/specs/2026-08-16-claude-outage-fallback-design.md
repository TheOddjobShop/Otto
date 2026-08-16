# Otto Local Fallback — Design Spec

**Date:** 2026-08-16
**Status:** Implemented (`feat/ollama-fallback`)
**Author:** brainstormed with user

## Problem

Otto is a wrapper around Claude Code, which is a network service behind a
subprocess. It fails for reasons that have nothing to do with the machine Otto
runs on:

- the Anthropic API is overloaded or rate-limiting (`429`, `overloaded_error`);
- auth expired and nobody has run `claude /login` since;
- the house internet is down;
- an `npm` update left the binary missing or broken.

In every one of those cases Otto replies `⚠️ Claude error: …` and nothing else.
On a home server that is a nuisance. On the `otto tui` voice loop it is worse:
you say "hey otto, what's on my calendar", he goes quiet for ten seconds, and
then reads an exit status out loud.

Meanwhile the same box already runs Ollama — it has to, because semantic memory
embeds every turn locally.

## Goal

When a Claude turn fails, answer it locally instead, and say so.

## Non-goals

- **Parity.** The local model has no tools and never will through this path.
  MCP is Claude Code's, not Ollama's.
- **Seamless failover.** The user must always know which brain answered. A
  degraded answer mistaken for a full one is worse than an error.
- **Local-first routing.** Claude is tried every turn. This is a backstop, not a
  cost optimizer.
- **Sharing Claude's session.** Ollama has no `--resume`; the fallback keeps its
  own short history and does not touch `session_id`.

## Where it goes: the Runner seam

`claude.Runner` is a two-method interface (`Run`, `WithEnv`) that everything
funnels through. `FallbackRunner` wraps it:

```
handler ──► FallbackRunner ──► execRunner ──► claude subprocess
                   │
                   └── on failure ──► OllamaFallback ──► POST /api/chat
```

Both paths write the same `Event` stream into the caller's channel, so the
handler, the token tracker, the activity log, the voice bridge and the TUI need
no changes and never learn a second brain exists. In particular the fallback
emits `AssistantTextEvent` **per streamed chunk**, not one blob at the end —
otherwise the voice path would sit silent until the whole reply had generated,
which on a 20B model is the entire wait.

Only Otto's own runner is wrapped. Toto and Toot are cosmetic — Toto exists to
cover for a *busy* Otto, and a Claude outage means Otto is failing, not busy —
so their failures stay failures rather than spending 13 GB of model on a cat.

## When to fall back

The bias is toward falling back, because the alternative the user sees is an
error they can do nothing about. Any primary failure qualifies, including a
`ResultEvent` whose subtype is not `success` (Run returns nil there, so the
wrapper watches the event stream as it passes).

Two exclusions, both because falling back would be actively wrong rather than
merely unnecessary:

| Case | Why not |
|---|---|
| Context cancelled | `/restart`, shutdown and the hang watchdog all cancel. Each is somebody deciding this turn should stop; answering it anyway ignores that. |
| Text already emitted | Claude can die partway through a reply that was largely fine. A second, worse answer appended to it leaves the user with two and no way to choose. |

`classifyFailure` names the cause (`claude auth failed`, `network unreachable`,
`claude binary missing`, …). Every branch falls back — the classification exists
for the log and for `/status`, because an outage is diagnosed from those lines
afterwards and "binary missing" and "rate limited" send you to very different
places.

## Honesty

Every fallback reply is prefixed:

> Heads up: Claude Code is unreachable, so this answer is from the local model
> (gpt-oss:20b) with no tools and no memory access.

Phrased as speakable prose rather than a bracketed tag, because it goes through
piper on the voice path. It leads the reply so it is the first thing read and
the first thing heard.

The system prompt gets a clause appended after Otto's own persona, time block
and memory core — those stay, since they are what make the reply sound like
Otto. The clause corrects only what is now false: the tools are gone, and
inventing an email it never sent is the specific failure to prevent.

## Conversation state

Ollama has no sessions, so without help every fallback turn is amnesiac — and an
outage lasting more than one question is the normal case. `OllamaFallback` keeps
three exchanges in memory (capped per reply so one runaway answer cannot crowd
out the rest). `/new` clears it alongside Otto's session, so the command means
one thing rather than two.

## Token accounting

The fallback emits `ResultEvent{Subtype: "success"}` with **zero** token counts.
The counts are real but local and free, and `/tokens` prices everything it
records against a Claude rate card — feeding them in would invent a dollar cost
for electricity. `ContextTokens` stays zero too, so the session rotator does not
read a fallback turn as growth in a Claude session that never ran.

## Availability

Checked per turn, not at boot: Ollama can be stopped, or have its model removed,
long after Otto starts, and a verdict cached at startup would answer a different
question than the one `/status` is asked. The probe is one `GET /api/tags`
against localhost with a short deadline.

The two ways it can fail are reported separately, because they send the user to
different places: an unreachable server means Ollama is not running, while a
reachable server missing the model means somebody has to run `ollama pull`.

When both brains fail, the error names both. The original Claude failure is
wrapped, so `errors.Is` still works on it.

## Install

`setup.sh` does **not** pull `gpt-oss:20b`. It is ~13 GB — an order of magnitude
above the embedding models — and Otto is fully usable without it. Setup prints
the one-line command, and `/status` says `UNAVAILABLE` with the same command
until it is there.

## Testing

`FallbackRunner` is tested through the event channel, which is the only seam
anything downstream sees: primary success never invokes the fallback, a primary
failure produces a labelled local reply, cancellation and partial replies are
left alone, and a doubly-broken system names both failures. `OllamaFallback` is
tested against an `httptest` server serving real NDJSON — streaming granularity,
zero token counts, history carried across turns and dropped by `ForgetHistory`,
and the system prompt actually disclaiming tools.
