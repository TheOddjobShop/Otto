# Otto Voice + TUI — Design Spec

**Date:** 2026-08-07
**Status:** Approved (design); implementation follows in this same series of PRs
**Author:** brainstormed with user
**Prior art:** `AbdurRazzaqBeta` (sibling repo) — a shipped Go voice stack this
design ports from, plus its own unmerged `voice-rearch` worktree whose spec
(`2026-04-24-jarvis-voice-tui-rearchitecture-design.md`) diagnosed the shipped
stack's structural flaws. Both are drawn on below; neither is adopted wholesale.

## Problem

Otto is text-only over Telegram. Two gaps follow.

1. **A phone keyboard is the wrong input for a machine you're sitting at.** The
   user runs an Arch PC. Speaking to Otto while working is strictly better than
   typing at a phone that then relays to the same machine.
2. **Otto has no face.** Everything he does — the router, rotator, bus drain,
   watchdog, pruner, activity log — is invisible unless you read the journal.

## Goal

`./otto tui` launches a front end: an always-on wake-word listener, a spoken
reply path, and a terminal UI. Telegram keeps working from the same process.
Every existing subsystem — memory, `session_search`, the model router, the agent
bus, the personas, token accounting — works over voice with no changes to its
own code.

## Non-goals

- **Multi-user or remote voice.** Same single-user allowlist; the mic is local.
- **Acoustic echo cancellation.** Real AEC is a CGO project. Barge-in is handled
  by transcript *content* instead (see "Barge-in").
- **Streaming/partial STT.** One whisper call per completed utterance. Partial
  transcripts are a v2 concern.
- **openWakeWord.** The `voice-rearch` spec is right that whisper-per-utterance
  is a heavy way to detect a wake word, but adopting openWakeWord means adding a
  Python runtime and venv to Otto's install story. Deferred, not rejected — the
  wake-detect seam is kept narrow so it can be swapped.
- **A server/client split.** The TUI process *is* Otto (see "Topology"). A socket
  transport between the two halves is a later addition this design does not block.
- **Voice for the `otto-memory` MCP server.** It stays a pure stdio server.

## Topology — one process

`./otto tui` runs the full Otto daemon with a terminal UI attached. It polls
Telegram *and* owns the mic.

The alternative — a thin TUI client talking to a separately-running daemon over a
Unix socket — is what `voice-rearch` chose, because that project had a server
laptop and a client PC. Otto has one machine. One process means no IPC, no
protocol, no reconnect logic, and no second binary, and Telegram keeps working
while the TUI is up because the same process is doing the polling.

### The consequence: a single-instance lock

Two processes long-polling Telegram with one bot token do **not** error. Telegram
hands each update to whichever poller asks first, so messages land in one process
or the other at random. The symptom is "Otto sometimes ignores me", and it is
miserable to diagnose.

So Otto takes an exclusive `flock` on `<state dir>/otto.lock` at startup, holding
it for the process lifetime. A second instance fails fast with the running PID
and the exact command to stop the other one. This applies to the plain daemon
too, not just the TUI — the hazard predates this feature; it was simply never
possible to hit before there were two ways to launch Otto.

## The surface mux

`telegram.BotClient` is already the surface abstraction. It is four methods, and
`cmd/otto/tty.go` already implements it for `-tty` mode. Every one of the 13
reply sites in the tree routes through
`telegram.SendChunked(ctx, <bot>, chatID, text)`.

So the front end needs no new interface — it needs a **multiplexer**:

```
                ┌──────────── muxBot (implements telegram.BotClient) ───────────┐
 Telegram ──────┤ GetUpdates:  merge both sources into one []Update stream      │
 TUI (voice ────┤ SendMessage: chatID == tuiChatID ? → TUI sink : → Telegram    │
  and typed)    └───────────────────────────────────────────────────────────────┘
                                          │
                              unchanged handler.dispatch
```

TUI-sourced updates carry a reserved `ChatID` (`tuiChatID`, a sentinel that can
never collide with a real Telegram chat id) and the allowlisted `UserID`, so
`auth.Allows` passes unchanged.

**What this buys:** `handler.go`, `toto.go`, `toot.go`, `commands.go`, and
`bus.go` need no changes. The memory core, `session_search`, `recent_turns`, the
model router, the agent bus, the watchdog, the activity log, session rotation and
`/tokens` all work over voice on day one. `/status` spoken aloud is free.

**Ordering.** `muxBot.GetUpdates` must not let a quiet Telegram long-poll (up to
its timeout) delay a TUI utterance. It selects across both sources with the
Telegram poll running in its own goroutine feeding a channel, so a spoken message
dispatches immediately.

**Failure isolation.** A Telegram outage must not take voice down. Poll errors
keep their existing backoff inside the Telegram half; the mux keeps serving TUI
updates throughout.

## Voice pipeline

```
mic (sox, 16kHz mono s16le)
  → 100ms frames + RMS  ──────────────────────────► LevelEvent (drives the UI)
  → adaptive-threshold VAD → utterance assembly
  → whisper.cpp (one call per utterance)
  → StripWakeWord → command text
  → muxBot injects an Update  ──► normal Otto turn ──► reply text
  → sentence splitter → piper (per persona voice) → playback
```

### Wake word

Ported from `AbdurRazzaq/internal/voice`, which already defaults to `"otto"` and
already carries the alias table this design needs: `auto`, `otter`, `oto`,
`oh no`, `arro`, `aura`, `auta`, `ado` — all observed whisper mis-hears — plus
Levenshtein-distance-1 matching on 3-to-5-character tokens, up to two leading
connectives skipped (`okay`, `hey`, `um`, …), and a special case where a
sole-token `"or"` counts but `"or we could go home"` does not.

The bias is deliberate and is preserved: a false positive costs one ignored turn;
a false negative means Otto silently didn't hear you, which is the failure that
makes a voice assistant feel broken.

### Barge-in

Otto says his own name in replies, and the mic hears the speakers. Naive
wake-word barge-in therefore self-triggers constantly — a bug `AbdurRazzaq` hit
and solved, and whose solution is ported verbatim in spirit:

While in `speaking` state, an utterance interrupts playback **only** if its
transcript matches a mute command (`shut up`, `quiet`, …) or a closer (`thanks`,
`that's all`, …). Anything else — including the wake word plus a fresh question —
is ignored and playback continues; the question is heard as a normal armed
follow-up once Otto stops talking.

Utterances also carry the state they *began* in, not the state at flush time, so
an utterance that started during playback is classified as loopback no matter how
long the silence detector took.

### Conversation shape

`idle → armed → processing → speaking → armed …`, plus a `muted` overlay.

- Wake word alone → a varied spoken ack ("Yes?", "Go ahead.") and `armed`.
- Wake word + command in one breath → straight to the turn.
- While `armed`, follow-ups need no wake word.
- Closers (`thanks`, `that's all`, `bye`) end the conversation with an ack and no
  model call at all.
- Mute is silent and instant; `otto wake up` (or `m` in the TUI) resumes.

Acks are pre-rendered to disk and keyed by `sha1(voice + phrase)` — the voice is
part of the key because this design gives each persona its own voice, and the
original single-voice cache would otherwise serve Toto's lines in Otto's voice.

## Reply length

The spoken channel cannot carry Otto's text register. Two mechanisms, both needed:

1. **`Source: "voice"`** on the turn. It flows through the existing `recordUsage`
   path, so `/tokens` grows a `voice` line for free, and through `logTurn` as a
   tag (below).
2. **A VOICE MODE clause** appended to the system prompt for voice turns: two or
   three spoken sentences, no lists, no markdown, no file paths or URLs read
   aloud, no code.

`SanitizeForTTS` ports over as the backstop that strips markdown, emoji, and
fences before text reaches piper — because a model told not to emit markdown
still sometimes does, and piper reads asterisks aloud.

**The scope trap.** `AbdurRazzaq/CLAUDE.md` carries a line worth restating here
because getting it wrong is subtle: *voice mode is about delivery format, not
scope.* A model told "be brief" will quietly start doing less work — skipping the
MCP call and just talking about it. Otto voice-asked to push something to Notion
must push it, then say one sentence about what he did. The prompt clause says so
explicitly.

`TOOT.md` needs the same clause: his persona currently licenses "two short
paragraphs", which is far too long spoken. `TOTO.md` needs nothing — "Brief — one
short paragraph, maybe two. Cats don't monologue" is already voice-shaped.

## Turn logging for voice

Voice turns log **the text that was actually spoken**, tagged as voice.

The alternative — generating a fuller text version to store — was rejected on two
grounds: it costs an extra model call on the one path where latency is the entire
product, and it would write into memory something Otto never said, which
contradicts the truthfulness the personas are built around.

The honest consequence, recorded here so a later reader doesn't mistake it for a
bug: **a voice conversation leaves a terser trail in memory than the same
conversation typed.** `session_search` and `recent_turns` over a voice-heavy
period return less material. That is accurate — the conversation *was* terser —
and the tag lets a future change weight or expand voice turns if this proves
annoying in practice.

## Per-persona voices

Each character gets its own piper model, resolved through a
`voice_models` config table with per-persona defaults. Otto keeps
`en_US-danny-low` (the voice `AbdurRazzaq` shipped). Toto and Toot get distinct
voices so the busy-handoff is audible: ask something while Otto is mid-task, and
a different voice answers in the cat's register.

This is not decoration. Otto's activity log already records what he is actually
doing, and `TOTO.md` already instructs Toto to summarize it impressionistically
("he's rerunning your tests"). Spoken, during a long silent task, that is the
single most useful thing the pet system produces.

Announce-mode Toot (release notes) stays text-only: a changelog is a list, and
lists are exactly what the voice channel is bad at.

## Platform

Arch Linux is the target. `cmd/otto` is already `//go:build unix`, so the build
tags hold. Three platform seams:

- **Playback** — `afplay` is macOS-only. Resolution order becomes
  `paplay` → `aplay` → `play` (sox) → `afplay`, first found wins.
- **piper install** — upstream ships `piper_linux_x86_64.tar.gz` /
  `piper_linux_aarch64.tar.gz` at the same release base as the macOS artifact.
  The dylib-path handling stays darwin-only; Linux uses `LD_LIBRARY_PATH`.
- **Mic capture** — `sox -d` takes the default ALSA/Pulse device. Device
  selection is the one thing that may need hands-on configuration, so
  `otto voice-doctor` reports precisely what is missing or unopenable.

## Install

`setup.sh` grows a voice section: `sox` and `whisper.cpp` via pacman (with a
build-from-source fallback, since `whisper.cpp` moves between repos), plus piper
and the model downloads. A normal install is therefore voice-ready.

If the TUI is launched on a machine that skipped it, `EnsureInstalled` downloads
what is missing with **visible progress** — roughly 500 MB for `ggml-small.en`
plus the voice models. Silent half-gigabyte downloads are a bad first impression;
a progress display is the whole difference.

## The TUI

Bubble Tea v2 (`charm.land/bubbletea/v2`), ported from `AbdurRazzaq/internal/tui`
with the academic-lockdown mode dropped — that was a feature of the other project.

- **Boot** — the `OTTO` reveal animation, then status lines.
- **Minimal mode** (default) — the lightbulb (a real 3D rasterizer with per-cell
  ANSI color and a rotating filament) above an audio-reactive bar cluster, with a
  single status line: `listening for "otto"…` / `go ahead, i'm listening` /
  `heard: "…"` / `thinking…` / `speaking: "…"` / `muted`.
- **Chat mode** — type any printable character to enter; scrollback plus input.
  Voice turns mirror into the same scrollback, so one transcript covers both.
- **Keys** — `m` mute toggle (on an empty input), `esc` back to minimal,
  `ctrl+c` quit.

### Streaming TTS

This is the one improvement taken from `voice-rearch` immediately, because Otto
can have it almost for free where `AbdurRazzaq` could not: Otto already parses
`claude.AssistantTextEvent` as a *stream*. The TUI sink accumulates streamed text,
splits on sentence boundaries, and hands each completed sentence to piper as it
arrives — so Otto starts speaking while still generating.

`voice-rearch` names sequential record→STT→LLM→TTS→play as its first structural
flaw. This removes the largest term in that sum without adding a process.

Sentence splitting must not fire on abbreviations or decimals mid-stream, and
must flush whatever remains when the stream ends. It is pure and unit-testable.

## Dependency impact

Charm v2 (bubbletea, bubbles, lipgloss) adds roughly 15 indirect dependencies to
a tree that currently has 5 direct. Contained to the `otto` binary; `otto-memory`
stays lean. This is a real change in character for a project that has kept its
dependency tree small deliberately, and is accepted for what the TUI provides.

## Testing

The audio round-trip cannot be tested in CI (no mic, no speakers, and the
development machine is not the target platform). The line is drawn at process
boundaries:

- **Pure and unit-tested**: wake-word matching across the full alias/fuzzy/
  connective grammar, mute/closer/unmute phrase matching, `SanitizeForTTS`,
  sentence splitting, RMS/VAD threshold logic, `pcmToWav` framing, mux routing by
  chat id, lockfile acquire/refuse, voice-keyed cache paths.
- **Fixture-tested**: recorded WAV fixtures through the utterance→transcript seam
  with a stub transcriber, so the state machine is exercised without audio
  hardware.
- **Stubbed**: piper and playback behind interfaces, as `claude.Runner` already
  is, so the speaking path is testable without sound.
- **Manual, documented in the README**: mic capture, real STT accuracy, real
  playback, and barge-in. `otto voice-doctor` exists to make the first failure
  legible rather than mysterious.

## Phasing

1. **Phase 0** — harden the existing bot: test CI, pet watchdog coverage,
   extended `/status`, doc drift. Tag `v1.0.0`. (Independent of voice; done first
   so CI protects everything after.)
2. **Phase 1** — `internal/voice`: whisper/piper wrappers, wake word, VAD,
   cache, installer, Linux paths. Telegram voice notes ride the same STT half.
3. **Phase 2** — `muxBot`, the lockfile, `Source: "voice"`, the VOICE prompt
   clause. Testable through `-tty` before any TUI exists.
4. **Phase 3** — `internal/artanim`, the Bubbletea model, `./otto tui`, streaming
   TTS.
5. **Phase 4** — proactive messages via the bus's `source="user"` path, store
   coverage, per-persona voices.

Each phase leaves Otto runnable and is independently testable.
