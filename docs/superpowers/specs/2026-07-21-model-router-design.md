# Otto Model Router — Design Spec

**Date:** 2026-07-21
**Status:** Retroactive — documents behavior as shipped in v0.7.2
**Author:** written after the fact from the code

## Problem

Otto handles two very different workloads through one Telegram thread: real
coding work (repos, builds, debugging) and ordinary assistant chat (reminders,
questions, planning, life admin). Pinning Otto to a coding-grade model makes
every "what's on my calendar" cost like a refactor; pinning him to a chat model
makes the coding turns worse than they need to be.

## Goal

Decide, per turn, which model Otto runs on — before the main subprocess is
spawned — using a call cheap enough that the decision never dominates the saving.

## Non-goals

- **Learning or adapting.** The classifier is a stateless one-shot; it has no
  session, no history, and no feedback from whether the routing was right.
- **More than two classes.** The verdict space is exactly `CODE` / `CHAT`.
- **Routing the pets.** Toto and Toot are pinned to Haiku
  (`toto.go:25`, `toot.go:25`); the router is Otto-only.
- **Routing on anything but the text.** Photos and attachments do not enter the
  classifier prompt; an empty text body short-circuits to the default model
  (`classify.go:148-150`).
- **Blocking a reply.** Every failure mode returns a model, never an error.

## The decision

`classifyPromptTmpl` (`cmd/otto/classify.go:47`) defines the whole taxonomy:

- **CODE** — writing, editing, debugging, reviewing, running, or explaining
  code; working in a git repo or codebase; build / test / deploy / lint; creating
  or changing files in a software project.
- **CHAT** — everything else: questions, planning, reminders, math, scheduling,
  life admin, casual conversation.

The prompt demands "EXACTLY one word and nothing else." Keeping it this tight is
deliberate: a small, constrained prompt is both cheap and hard to drift.

## The three tiers

```go
ottoDefaultModel = "claude-sonnet-4-6" // ordinary chat
ottoCodingModel  = "claude-opus-4-8"   // coding tasks — the router escalates here
classifierModel  = "claude-haiku-4-5"  // the router itself
```
(`classify.go:26-28`)

- **Sonnet as the default** — balances quality against a larger context window,
  which matters because Otto's session is `--resume`d across turns and grows.
- **Opus on escalation** — the expensive tier is entered only when code work
  actually warrants it.
- **Haiku for the router** — the router runs on *every* turn, so it must be the
  cheapest and fastest thing in the pipeline or it eats the saving it exists to
  produce.

`modelLabel` (`classify.go:118`) renders these for `/status`:
`"opus-4.8 (coding)"`, `"sonnet-4.6 (chat)"`, and `"default (inherited)"` for
the empty-model case.

A `nil` classifier on the handler means "don't route" — `RunArgs.Model` stays
empty and Otto inherits Claude Code's own default. This keeps bare configs and
tests working without a router (`classify.go:39-42`).

## Fail cheap

Every failure path returns `ottoDefaultModel`:

- empty message after trimming (`classify.go:148`)
- subprocess error or timeout — logged as `model router failed (…)`
  (`classify.go:186-189`)
- JSON that will not unmarshal — logged as `unparseable JSON`
  (`classify.go:190-194`)
- a parsed verdict that is not `CODE` — including `CHAT`, empty output, and
  noise (`parseModelFromVerdict`, `classify.go:73`)

`parseModelFromVerdict` upper-cases the raw text, splits on non-`A–Z`, and
escalates only when the **first alphabetic token** is exactly `CODE`. The
asymmetry is the point, and the doc comment says so: the failure mode of a
misrouted turn should be "too cheap," not "too expensive." Routing must never
block or break a reply.

`classifyTimeout = 20s` (`classify.go:34`) bounds the whole thing, and expiry is
just another path to the default.

## Prompt-injection hardening

The user's message is interpolated into a delimited block:

```
User message:
<<<
%s
>>>
```

`classifyPrompt` (`classify.go:59`) rewrites every literal `>>>` in the message
to `> > >` before formatting, so a user (or, more realistically, quoted content
the user pasted) cannot close the data block early and append instructions that
steer the router. The comment is candid about proportionality: the blast radius
is only model selection, but the defang is one `strings.ReplaceAll` and worth
having. Note the opening `<<<` is not defanged — only the terminator can end the
block.

## Why not Otto's main runner

`execClassifier` (`classify.go:136`) builds its own `exec.CommandContext` rather
than going through `claude.Runner`. The router needs **none** of what the runner
provides:

- **No MCP.** No `--mcp-config` is passed, so no Gmail/Notion/Calendar/
  otto-memory servers are initialized. Since MCP init is a large fraction of a
  cold `claude` start, skipping it is most of why the router is cheap.
- **No session.** `--no-session-persistence` — every classify is a fresh
  one-shot, and without the flag each Telegram message would litter a new
  session file on disk. The comment notes this mirrors the proven
  prayer-checkin scripts.
- **No tools.** `--disallowedTools "*"`. The router emits one word; a tool call
  would be a bug.

It also passes `--dangerously-skip-permissions` (same threat model as the rest
of Otto: the allowlist gate is upstream), `--output-format json`, and pins
`cmd.Dir` to Otto's `workDir` (`$HOME`) and `OTTO_RUNNING=1` into the env.

## Process-group kill and WaitDelay

Two subprocess-hygiene measures, mirroring `internal/claude/runner.go`:

- `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}` plus a `cmd.Cancel`
  that `SIGKILL`s the whole group (`syscall.Kill(-pid, SIGKILL)`), so anything
  `claude` spawned — hooks, stdio children — is reaped too rather than
  orphaned. `ESRCH` (group already gone) is translated to `os.ErrProcessDone`,
  because per `os/exec` any other error from `Cancel` is surfaced by `Wait` even
  on a clean exit, which would misreport that benign race as a router failure
  (`classify.go:169-179`).
- `cmd.WaitDelay = 5 * time.Second` — even after the group kill, an escaped
  descendant holding the stdout pipe open would make `Output()` block forever,
  wedging the Otto slot until restart (`classify.go:180-183`).

## Token accounting

The router is a Claude call and is billed like one, so it reports its own usage.
`--output-format json` returns a single envelope carrying both the assistant's
text and a `usage` block; `parseClassifyJSON` (`classify.go:95`) extracts
`result` plus `input_tokens`, `output_tokens`, `cache_creation_input_tokens`,
`cache_read_input_tokens` into a `classifyUsage` whose field names mirror
`claude.ResultEvent` so the shared `recordUsage` helper consumes it unchanged.

One row is written with `source = "classify"` and `model = claude-haiku-4-5`
(`classify.go:195`), surfacing in `/tokens` as its own line. Recording happens
*after* a successful parse and *before* the verdict is mapped, so an
unparseable envelope records nothing and still returns a model. `recordUsage`
itself is best-effort — a nil store or a write error is logged and swallowed
(`cmd/otto/usage.go:18`). The classifier's store handle is optional
(`execClassifier.store`, nil disables recording).

## Call sites

Both Otto turn paths route identically, immediately before `runAndReply`:

- `cmd/otto/handler.go:555` — the Telegram path. The classify call runs while
  Otto **already holds the slot**, so a message arriving during classification
  still falls back to Toto rather than queueing behind the router.
- `cmd/otto/bus.go:179` — the agent-bus path, same shape.

Both then call `h.otto.setModel(model)` so `/status` can report the model of the
most recent turn.

## Testing

`cmd/otto/classify_test.go` covers `parseModelFromVerdict` (CODE, CHAT, mixed
case, leading punctuation/noise, empty), the `>>>` defang in `classifyPrompt`,
and `parseClassifyJSON` on a well-formed envelope (verdict + all four usage
fields) versus malformed input returning `ok == false`.

## Affected files

- `cmd/otto/classify.go` — the whole router.
- `cmd/otto/handler.go` — Telegram-path call site, `setModel`.
- `cmd/otto/bus.go` — bus-path call site.
- `cmd/otto/main.go:185` — construction of the production `execClassifier`.
- `cmd/otto/usage.go` — `recordUsage`, shared with the runner-based sources.
