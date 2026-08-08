# Otto

Single-user Telegram bot wrapping Claude Code with MCP tools (Notion + Gmail + Google Drive + Google Calendar) **plus a local persistent-memory system** (curated core + semantic recall over conversation history). Runs as a `systemd --user` service (Arch Linux) or launchd user agent (macOS). Conversation memory survives messages, restarts, and `/new`.

Design specs:

- [`docs/superpowers/specs/2026-04-24-otto-design.md`](docs/superpowers/specs/2026-04-24-otto-design.md) — original bot design.
- [`docs/superpowers/specs/2026-05-27-otto-memory-rearchitect-design.md`](docs/superpowers/specs/2026-05-27-otto-memory-rearchitect-design.md) — the memory system (Hermes-style two-tier + local embeddings + idle rotation).
- [`docs/superpowers/specs/2026-07-21-agent-bus-design.md`](docs/superpowers/specs/2026-07-21-agent-bus-design.md) — the inter-agent message bus (inbox table, drain loop, hop cap). *Retroactive.*
- [`docs/superpowers/specs/2026-07-21-model-router-design.md`](docs/superpowers/specs/2026-07-21-model-router-design.md) — the per-turn Haiku model classifier (CODE vs CHAT routing). *Retroactive.*
- [`docs/superpowers/specs/2026-07-21-pets-and-watchdog-design.md`](docs/superpowers/specs/2026-07-21-pets-and-watchdog-design.md) — the multi-persona pet system (Toto/Toot) and the liveness watchdog. *Retroactive.*
- [`docs/superpowers/specs/2026-08-07-voice-tui-design.md`](docs/superpowers/specs/2026-08-07-voice-tui-design.md) — the surface mux, the local voice pipeline (wake word → whisper → Otto → piper), and the `./otto tui` front end.

## Quick start

```bash
./setup.sh
```

The script is idempotent — re-run anytime to add credentials, install missing pieces, or fix things; it skips what's already done. It detects Arch Linux (`pacman`) or macOS (`brew`) and adapts; the systemd unit is Linux-only. It will:

1. Install system deps (`go`, `nodejs`, `npm`, `jq`, `curl`, `python`, `lsof`, plus `base-devel` on Arch).
2. Install Claude Code CLI globally via `npm`.
3. Build the `otto` binary and the **`otto-memory`** MCP server binary into `~/.local/bin/`.
4. Create the memory directory (`~/.local/state/otto/memory/`) and state DB path.
5. **Install Ollama** (pacman/brew), start its service, and **pull `embeddinggemma` + `nomic-embed-text`** for local semantic embeddings. Best-effort — if Ollama install or model pull fails, memory search degrades to keyword-only (FTS5) and setup continues.
6. Walk through one-time Google Cloud Console setup (OAuth Desktop client).
7. Browser sign-ins for Google Calendar, Drive, Gmail.
8. Prompt for Notion integration token, Telegram bot token + your Telegram user ID. (Anthropic auth is delegated to Claude Code — see "Claude Code authentication" below.)
9. Write `~/.config/otto/{config.toml, mcp.json, client_secret.json}` with `0600` perms. `mcp.json` registers the local `otto-memory` server alongside the community MCPs.
10. On Linux, install a `systemd --user` service at `~/.config/systemd/user/otto.service`, enable lingering, start the service, and tail the journal briefly to confirm it's healthy. On macOS, install a launchd user agent at `~/Library/LaunchAgents/com.otto.bot.plist` (with `KeepAlive=true` so `/update`'s SIGTERM auto-respawns) and bootstrap it into the current session.

## Manual smoke test

After `setup.sh` reports success, on Telegram:

- Send `hi` — Otto replies.
- Send `My name is Alice.` then `What's my name?` — second reply should know "Alice" (persistent session).
- Send `Remember that I prefer light mode in VS Code.` — Otto calls `memory_add` and the fact lands in `MEMORY.md`.
- Send `/new` then `What's my name?` — should not remember (session cleared); but `Do you know my VS Code preference?` should still answer "light mode" because the memory core survives `/new`.
- Send `What did we talk about earlier?` — Otto calls `session_search` (semantic + keyword) over `state.db`.
- Send `/whoami` — prints your Telegram user ID and current session ID.
- Send `/status` — prints uptime, busy/idle state, the model the last turn ran on, the session id, and the state of the background machinery: how full the session is and how long until it rotates, whether embeddings are actually working (from the last real attempt, not a probe), when the store was last pruned and how many rows went, and how many bus messages are queued vs. ready to deliver now. Every lookup is in-memory or one indexed `COUNT`, so it stays fast even when something is wrong.
- Send `/restart` — interrupts an in-flight Claude call.
- Send `/tokens` — prints all-time token usage with a per-source breakdown (main / bus / toto / toot / classify / flush), plus an estimated dollar cost broken down by model. The cost is computed from published list prices in `cmd/otto/pricing.go` and assumes the default 5-minute cache TTL; it is an estimate, not a billing figure, and any model without a rate card (e.g. turns that inherited Claude Code's own model) is named as excluded rather than silently counted as free.
- Send a photo with caption "describe this" — Otto downloads it and forwards to Claude via `@<path>`.
- Hold the microphone button and say "what's on my calendar today" — Otto transcribes it locally with whisper.cpp and answers as though you had typed it. Commands and pet addressing work too: saying "toto, what's otto up to" routes to the cat. The reply comes back as text, since you're looking at Telegram. Requires `whisper.cpp` plus a model, and `ffmpeg` to decode Telegram's Opus — `otto voice-doctor` reports exactly what's missing.
- Send "what's on my calendar today?" — exercises the Google Calendar MCP.

## Voice

Otto listens and speaks entirely locally — [whisper.cpp](https://github.com/ggerganov/whisper.cpp)
for speech-to-text, [piper](https://github.com/rhasspy/piper) for text-to-speech.
No API keys, no per-token cost, nothing leaves the machine. Same reasoning that
put embeddings on a local Ollama.

Two ways in:

- **Telegram voice notes** — hold the mic button. Transcribed and handled as
  text; the reply comes back as text, because you're looking at a screen.
- **`./otto tui`** — a terminal front end with an always-on wake word. Say
  "otto" and he answers out loud. See the section below.

```bash
otto tui               # the front end: wake word, spoken replies, lightbulb
otto voice-doctor      # check every piece and print exactly what's missing
```

### `otto tui`

A boot reveal, then a spinning lightbulb over an audio-reactive bar cluster and
one line of status. Say **"otto"** and he answers out loud.

| | |
|---|---|
| `otto` | wake word. Say it alone for an acknowledgment, or with your request in the same breath |
| — | once he's answered you're still armed: follow-ups need no wake word |
| "thanks" / "that's all" / "bye" | ends the conversation. No model call — it's a fast path |
| "otto, shut up" | mutes instantly, killing playback mid-sentence |
| "otto, wake up" | unmutes |
| any key | type to open the chat pane; `esc` closes it and keeps your draft |
| `m` | mute toggle (on an empty input) |
| `ctrl+c` | quit |

Otto starts speaking **before he finishes thinking** — his reply is split into
sentences as it streams, so the first one plays while the rest is still being
generated.

While he's speaking, only "shut up" and closers interrupt. A question asked over
him waits and is heard as a follow-up once he stops. That's deliberate: Otto says
his own name in replies and the microphone hears the speakers, so anything less
selective self-interrupts constantly.

**One process at a time.** `otto tui` *is* Otto — it polls Telegram too, so your
phone keeps working while the UI is up. That means you can't run the service at
the same time: two pollers sharing one bot token would split your messages
between them at random. Otto takes a lock at startup and refuses with the exact
command to stop the other one.

```bash
systemctl --user stop otto     # then: otto tui
```

Logs go to `<state dir>/tui.log` while the UI owns the terminal.

`voice-doctor` is the first thing to run when voice misbehaves. It checks sox,
whisper, piper, the decoder, playback, every model file (including the
`.onnx.json` sidecars piper needs but never names in its own errors), and opens
the microphone to confirm real samples arrive — reporting the `pacman` line for
whatever is absent.

### Models

Assets live in `<state dir>/voice/` and download on first use, with progress:

| Asset | Size | Purpose |
|---|---|---|
| `ggml-small.en.bin` | ~466 MB | speech-to-text. `base.en` or `tiny.en` are honored if already present |
| `en_US-danny-low` | ~20 MB | Otto's voice |
| `en_US-amy-low` | ~20 MB | Toto's voice — lighter and quicker |
| `en_US-lessac-low` | ~20 MB | Toot's voice — crisper, more clipped |
| piper binary | ~20 MB | the synthesizer itself |

Each persona gets a distinct voice on purpose: when Otto is mid-task and Toto
covers for him, you hear that it's someone else without being told.

## Scheduled work — `otto say`

```bash
otto say "good morning — give me today's brief"
otto say -to toot "what version are we on?"
echo "$(cat report.txt)" | otto say
```

Hands a message to the running Otto, who answers it with full context and logs
the exchange to memory like any other turn. This is how a launchd job, systemd
timer or cron entry should reach you.

The distinction matters. A script that posts to the Telegram API directly sends
text Otto never saw: it isn't in his session, isn't in `recent_turns`, and when
you reply "yes, do that" he has nothing to resolve it against. Through `otto
say`, the briefing *is* his turn, so replying to it just continues the
conversation.

`-timeout` waits for the daemon to pick the message up, which is how a script
can tell Otto is actually running — the enqueue itself succeeds either way,
since the queue is just a table.

## Operations

```bash
journalctl --user -u otto -f          # tail logs
systemctl --user status otto          # check status
systemctl --user restart otto         # restart
systemctl --user stop otto            # stop

systemctl --user status ollama        # Ollama service (semantic embeddings)
ollama list                           # which embedding models are pulled
```

### Service (macOS)

```bash
launchctl list | grep otto                          # confirm loaded
launchctl print gui/$(id -u)/com.otto.bot           # full status
launchctl kickstart -k gui/$(id -u)/com.otto.bot    # restart
tail -f ~/Library/Logs/otto.log                     # logs
```

## Build / test

```bash
make build              # builds ./otto
make test               # unit tests for all packages (uses testdata/fake-claude.sh as a stub claude)
make vet                # go vet + gofmt check
go test -race ./...
go build ./cmd/otto-memory   # build the MCP server binary
```

CI runs the same checks on every push to `master` and every pull request
(`.github/workflows/test.yml`): gofmt, `go vet`, `go build`, and `go test -race`,
plus a cross-build matrix over the four release targets (linux/darwin ×
amd64/arm64) so a platform-specific break is caught without needing those
runners. `.github/workflows/release.yml` is separate and fires only on `v*` tags.

## Layout

```
.
├── cmd/
│   ├── otto/             # bot daemon: handler, commands, personas (Toto, Toot),
│   │                     # markdown stripper, updater, watchdog, idle-gated rotator,
│   │                     # surface mux, voice bridge, instance lock
│   └── otto-memory/      # MCP stdio server exposing memory_add/replace/remove + session_search
├── internal/
│   ├── voice/            # local speech: whisper.cpp STT + piper TTS, wake word,
│   │                     # VAD, barge-in, streaming sentence split, diagnostics
│   ├── tui/              # Bubble Tea front end: minimal (art) and chat modes
│   ├── artanim/          # 3D lightbulb rasterizer, Siri bars, boot reveal
│   ├── auth/             # single-user allowlist
│   ├── config/           # TOML config loader
│   ├── telegram/         # Bot API wrapper, chunking, image download
│   ├── claude/           # subprocess Runner + stream-json parser + session ID persistence
│   ├── store/            # SQLite turn log + FTS5 keyword search + vectors table + semantic search
│   ├── memory/           # bounded curated-memory core (USER.md/MEMORY.md): load/inject,
│   │                     # add/replace/remove, security scan, capacity guard, RWMutex-safe
│   └── embed/            # local text embeddings (Ollama /api/embed) + ordered fallthrough Chain
│                         # + cosine similarity (brute-force top-k at single-user scale)
├── systemd/otto.service  # user-service template (Linux only)
├── launchd/com.otto.bot.plist  # user-agent template (macOS only)
├── setup.sh              # idempotent installer (Arch + macOS), incl. Ollama + model pull
├── testdata/             # fake-claude.sh stub claude used by tests
├── docs/superpowers/
│   ├── specs/            # design specs (original + memory rearchitect)
│   └── plans/            # implementation plans (one per merged PR)
├── SYSTEM.md             # Otto's persona — copied to ~/.config/otto/system_prompt.md
├── TOTO.md               # Toto's persona (cat, busy-fallback + name-addressed chat)
├── TOOT.md               # Toot's persona (owl, release announcer + chat)
└── go.mod                # Go 1.26; deps: BurntSushi/toml, go-telegram-bot-api/v5,
                          # modernc.org/sqlite (pure-Go), modelcontextprotocol/go-sdk,
                          # charm.land/bubbletea+bubbles+lipgloss v2 (TUI only)
```

## Memory system

Otto persists memory across messages, restarts, and `/new`. Two tiers (Hermes-style):

**Tier 1 — Curated core (always injected into every prompt).** Two bounded markdown files Otto can read directly as part of his system prompt:

- `~/.local/state/otto/memory/USER.md` — identity, preferences, communication style (cap ~500 tokens).
- `~/.local/state/otto/memory/MEMORY.md` — environment facts, project conventions, lessons (cap ~800 tokens).

`setup.sh` seeds `MEMORY.md` on a fresh install with the deployment facts it has just established first-hand (OS + service manager, where config and state live, where scheduled scripts belong, which MCP servers are wired) and offers to seed `USER.md` with what Otto should call you. Seeding is strictly additive: a file that already exists and is non-empty is never touched, so re-running `setup.sh` cannot clobber curated memory.

Hand-editable. The `otto-memory` MCP server exposes `memory_add` / `memory_replace` / `memory_remove` so Claude can update them mid-conversation. Every write is security-scanned (credentials / prompt-injection patterns / invisible Unicode are rejected) and exact duplicates are blocked. At 80% of cap the next `memory_add` errors with the current contents, prompting consolidation.

**Tier 2 — Episodic + semantic store (on-demand, bounded).** A SQLite database at `~/.local/state/otto/state.db`:

- `turns` table — every Otto/Toto/Toot exchange (user + assistant). Rows are never updated in place, but an hourly pruner caps the table at the most-recent ~2000 turns (weeks of history for a single-user bot), cascading deletes to `turns_fts` and `vectors`.
- `turns_fts` — FTS5 keyword mirror, kept in sync by trigger.
- `vectors` table — one embedding per turn (model + dim + blob).

A background pruner runs hourly (and once at startup): it holds `turns` to the most-recent ~2000 rows and delivered inbox rows to the most-recent ~500, so the store stays bounded rather than growing forever.

**Recency vs. relevance.** `session_search` ranks by meaning and keyword — good for "what did we decide about the database?". It is the wrong tool for a follow-up that carries no content to rank: "did that work?", "do the second one", "yeah go ahead". Because the session is cleared after a period of inactivity, those arrive with no context on Otto's side even though they read as obvious to you. `recent_turns(limit=6)` returns the last few messages verbatim and in order, which is what actually resolves the reference. Otto is instructed to reach for it before answering anything referential, and to resolve it silently rather than announcing that his session reset. Toto and Toot can call it too, so they can see the same conversation you do.

Otto embeds each turn asynchronously after sending the reply (best-effort, 130 s-bounded — 2 × 60 s per embedding backend + 10 s slack — off the reply path). The `session_search` MCP tool embeds the query, runs semantic top-k + FTS5 in parallel, and merges (semantic first, keyword fills). If Ollama is unreachable or no model is pulled, search transparently degrades to FTS5 keyword only — nothing breaks.

**Semantic embeddings.** Local-only via Ollama at `http://localhost:11434`, ordered chain `embeddinggemma → nomic-embed-text → keyword floor`. Zero per-token cost, fully private. Vectors are tagged with `model` + `dim`, so a model swap silently ignores stale-dimension rows until they get re-embedded.

**Idle-gated session rotation.** Otto tracks `usage.input_tokens` per turn. A long-lived rotator goroutine clears the session when either of two conditions is met: (a) you have been quiet for **15 minutes** (idle reset — fires regardless of session size), or (b) `input_tokens` has crossed **85 %** of the model's context window AND you have paused for at least 5 minutes (hard cap with active grace, so the cap never wipes context mid-conversation). Before clearing, Otto runs a **flush pass** (`rotate_flush`, on by default): one cheap Haiku turn reviews the closing session and saves anything durable — a stated preference, an environment fact, a correction — into the memory core via `memory_add`. It is deliberately narrow: `memory_add` only (never replace or remove, so a background pass can't overwrite something you taught Otto deliberately), skipped for sessions under 5000 tokens, capped at three facts, bounded at 90 seconds, and failure just logs and lets the rotation proceed. Your next message then starts fresh, seeded by the always-injected memory core + retrieved memories. Continuity comes from the core (durable facts you've taught Otto) + `session_search` (any past turn). You should rarely need `/new` again.

**Deferred hand-off.** When Toto forwards something to Otto while Otto is mid-turn, the message is returned to the inbox with a retry time rather than dropped, and delivered as soon as Otto is free. Toto tells you about the wait once, on the first deferral — not once per retry. If Otto stays busy across the full retry window (~10 minutes, matching the watchdog's hang kill), Otto reports the failure to you in plain text rather than letting a hand-off you watched Toto accept disappear silently.

All four memory tools are exposed via the local `otto-memory` MCP server registered in `mcp.json`. Toto and Toot share read access to the core (so they know your prefs too); Otto is the only one that writes.

## Tool permissions

Otto invokes `claude` with `--dangerously-skip-permissions`. Claude Code's normal interactive permission prompt has no surface in `-p` mode, so it would otherwise reject every MCP tool call (`mcp__gmail__*`, `mcp__otto-memory__*`, etc.) and every `Bash`/`Write` invocation. The flag bypasses that gate entirely.

The threat model that makes this acceptable: Otto is a single-user bot on your own server, every incoming message is checked against `telegram_allowed_user_id` before reaching Claude, and you're texting your own bot from your own phone. Anyone with bot-token + allowlisted-user-ID could in theory spoof Telegram, but that's a different security perimeter — Otto's gate is at the Telegram allowlist.

When a tool call still slips through and is denied (Claude returns `permission_denials` on its result event), Otto surfaces it as a Telegram message naming the tool and the permission pattern to add to `~/.claude/settings.json`'s `permissions.allow` array.

## Claude Code authentication

Otto does not manage Anthropic credentials. Each `claude` subprocess inherits whatever auth Claude Code is already configured with. Set up auth once before running `setup.sh` using any of:

- `claude /login` — browser-based (Pro/Max subscriptions or API console). Stores in `~/.claude/`.
- `claude setup-token` — non-interactive long-lived token. Good for headless servers.
- `export ANTHROPIC_API_KEY=sk-ant-...` — explicit API key in the shell env.

If you use the env-var path (option 3), the systemd user service won't inherit your shell's environment by default. Add it to the unit:

```ini
[Service]
Environment=ANTHROPIC_API_KEY=sk-ant-...
```

For options 1 and 2, no systemd config is needed — `~/.claude/` lives under the user's home, which the user service reads natively.

## MCP servers

`setup.sh` writes `~/.config/otto/mcp.json` registering five servers:

- **`otto-memory`** — local Go binary at `~/.local/bin/otto-memory` (this repo). Tools: `memory_add`, `memory_replace`, `memory_remove`, `session_search`, `recent_turns`, plus the inter-agent bus tools `forward_to_otto`, `message_toto`, `message_toot`.
- `notion` — `@notionhq/notion-mcp-server` via `npx`.
- `google-calendar` — `@cocal/google-calendar-mcp` via `npx`.
- `gdrive` — `mcp-gdrive-workspace` via `npx`.
- `gmail-<label>` (one per Gmail account) — `@gongrzhe/server-gmail-autoauth-mcp` via `npx`.

Adding/removing servers: edit `mcp.json` and `systemctl --user restart otto`. On macOS use `launchctl kickstart -k gui/$(id -u)/com.otto.bot` instead.

### Version pinning

The four community servers are **pinned to exact versions**, declared once in `setup.sh` as `MCP_VER_NOTION` / `MCP_VER_GCAL` / `MCP_VER_GDRIVE` / `MCP_VER_GMAIL` and used both by the setup-time OAuth flows and by the generated `mcp.json`. Unpinned, every bot restart re-resolves `latest` and runs whatever the registry serves at that moment — in processes that are handed live OAuth client credentials through their environment. Each server is also spawned with `--ignore-scripts` so npm lifecycle hooks never execute on install.

To upgrade: read the package's changelog, bump the `MCP_VER_*` value in `setup.sh`, and re-run `./setup.sh` (it rewrites `mcp.json`). Pinning does not apply to the Claude Code CLI itself, which is installed globally via `npm i -g` — see the security note above that block.

## Config keys (`~/.config/otto/config.toml`)

All written by `setup.sh`. The memory/embed/rotation keys have sensible defaults; explicit values in `config.toml` override them.

| Key | Default | Notes |
|---|---|---|
| `telegram_bot_token` | required | from @BotFather |
| `telegram_allowed_user_id` | required | your Telegram numeric user id |
| `claude_binary_path` | required | autodetected |
| `mcp_config_path` | required | `~/.config/otto/mcp.json` |
| `session_id_path` | required | `~/.local/state/otto/session_id` |
| `system_prompt_path` | optional | copied from `SYSTEM.md` |
| `toto_persona_path` | optional | copied from `TOTO.md` |
| `toto_session_id_path` | `<session_id_path>_toto` | Toto's own session, so his history never mixes with Otto's |
| `toot_persona_path` | optional | copied from `TOOT.md` |
| `toot_session_id_path` | `<session_id_path>_toot` | Toot's own session |
| `memory_dir` | `<session dir>/memory` | USER.md + MEMORY.md live here |
| `state_db_path` | `<session dir>/state.db` | turn log + vectors |
| `embed_ollama_url` | `http://localhost:11434` | local Ollama |
| `embed_models` | `["embeddinggemma","nomic-embed-text"]` | ordered fallthrough |
| `model_context_tokens` | `200000` | denominator for rotation thresholds |
| `rotate_hard_pct` | `0.85` | rotate when tokens ≥ this fraction of context and user has paused ≥ 5 min |
| `rotate_idle_minutes` | `15` | minutes of silence after which the session clears regardless of size |
| `rotate_flush` | `true` | run a cheap Haiku pass over a session before clearing it, saving durable facts to the memory core |

### Caveman skill (or other SessionStart prose-changers)

Otto sets `OTTO_RUNNING=1` on every Claude Code subprocess. If you have a
SessionStart hook (e.g. the caveman skill) that alters reply tone, gate it
on this env var so it only fires in your IDE, not in Otto's replies:

    [ -n "$OTTO_RUNNING" ] && exit 0

Otto's personas also carry an explicit IGNORE-CAVEMAN instruction as a
backstop, so this hook patch is optional but cleaner.

## Troubleshooting

- **`go build` fails:** check `go.mod`'s `go` directive matches your installed Go (`go version`).
- **Otto not starting:** `journalctl --user -u otto -n 100`. Common causes: missing `~/.config/otto/client_secret.json`, missing `claude` on PATH (the unit's `Environment=PATH=` covers `~/.local/bin` and standard locations).
- **Telegram messages not arriving:** check the bot token in `config.toml`, and that `telegram_allowed_user_id` matches the user you're texting from. Non-allowlisted users are silently dropped.
- **Google auth expired:** re-run `setup.sh`; it will re-prompt for whichever credential is missing.
- **Memory not persisting facts:** confirm `otto-memory` is in `mcp.json` (`grep otto-memory ~/.config/otto/mcp.json`) and that `~/.local/state/otto/memory/` is writable. Logs print `turn log` / `embed turn` errors at the `otto` journal.
- **Semantic search not working:** check Ollama (`systemctl --user status ollama` on Linux, `brew services list` on macOS) and `ollama list`. Without a pulled embedding model, `session_search` falls back to keyword (FTS5) and logs `session_search: embed unavailable, keyword-only`. To enable semantic recall after a fresh install: `ollama pull embeddinggemma`.
- **Session never rotates:** the rotator fires when idle ≥ `rotate_idle_minutes` (regardless of session size), OR when `input_tokens` ≥ `rotate_hard_pct × model_context_tokens` AND you have paused for at least 5 minutes — whichever comes first. Otto must also be free (not mid-turn). The journal logs `rotator: rotated session ...` on success. Rotation cannot be disabled; to make it fire less often, raise `rotate_idle_minutes` and/or `model_context_tokens` (values ≤ 0 for either are reset to their defaults).
- **Claude `@<path>` image syntax wrong:** if images don't work, check `internal/claude/runner.go` and adjust against the installed Claude Code version's CLI.
- **`/update` seems to hang:** after `/update`, the bot exits within ~10s and systemd brings up the new binary on the next tick. Toot pings you back from the fresh process once it's settled. If you don't see that ping within ~30s, check `systemctl --user status otto` and `journalctl --user -u otto -n 50`.
- **macOS: `/update` leaves Otto offline:** check `launchctl print gui/$(id -u)/com.otto.bot` and confirm `KeepAlive = true`. Older hand-written plists may be missing it (or have the default `ThrottleInterval = 10` that swallows back-to-back `kickstart` calls) — re-run `./setup.sh` to install the canonical one.

## License

Private / personal use.
