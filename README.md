# Otto

Single-user Telegram bot wrapping Claude Code with MCP tools (Notion + Gmail + Google Drive + Google Calendar) **plus a local persistent-memory system** (curated core + semantic recall over conversation history). Runs perpetually as a `systemd --user` service on an Arch Linux home server. Conversation memory survives messages, restarts, and `/new`.

Design specs:

- [`docs/superpowers/specs/2026-04-24-otto-design.md`](docs/superpowers/specs/2026-04-24-otto-design.md) — original bot design.
- [`docs/superpowers/specs/2026-05-27-otto-memory-rearchitect-design.md`](docs/superpowers/specs/2026-05-27-otto-memory-rearchitect-design.md) — the memory system (Hermes-style two-tier + local embeddings + idle rotation).

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
10. On Linux, install a `systemd --user` service at `~/.config/systemd/user/otto.service`, enable lingering, start the service, and tail the journal briefly to confirm it's healthy.

## Manual smoke test

After `setup.sh` reports success, on Telegram:

- Send `hi` — Otto replies.
- Send `My name is Alice.` then `What's my name?` — second reply should know "Alice" (persistent session).
- Send `Remember that I prefer light mode in VS Code.` — Otto calls `memory_add` and the fact lands in `MEMORY.md`.
- Send `/new` then `What's my name?` — should not remember (session cleared); but `Do you know my VS Code preference?` should still answer "light mode" because the memory core survives `/new`.
- Send `What did we talk about earlier?` — Otto calls `session_search` (semantic + keyword) over `state.db`.
- Send `/whoami` — prints your Telegram user ID and current session ID.
- Send `/status` — prints uptime + session.
- Send `/restart` — interrupts an in-flight Claude call.
- Send a photo with caption "describe this" — Otto downloads it and forwards to Claude via `@<path>`.
- Send "what's on my calendar today?" — exercises the Google Calendar MCP.

## Operations

```bash
journalctl --user -u otto -f          # tail logs
systemctl --user status otto          # check status
systemctl --user restart otto         # restart
systemctl --user stop otto            # stop

systemctl --user status ollama        # Ollama service (semantic embeddings)
ollama list                           # which embedding models are pulled
```

## Build / test

```bash
make build              # builds ./otto
make test               # unit tests for all packages
make test-integration   # integration tests (uses testdata/fake-claude.sh as a stub claude)
make vet                # go vet + gofmt check
go test -race ./...
go build ./cmd/otto-memory   # build the MCP server binary
```

## Layout

```
.
├── cmd/
│   ├── otto/             # bot daemon: handler, commands, personas (Toto, Toot),
│   │                     # markdown stripper, updater, watchdog, idle-gated rotator
│   └── otto-memory/      # MCP stdio server exposing memory_add/replace/remove + session_search
├── internal/
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
├── setup.sh              # idempotent installer (Arch + macOS), incl. Ollama + model pull
├── testdata/             # fake-claude.sh stub for integration tests
├── docs/superpowers/
│   ├── specs/            # design specs (original + memory rearchitect)
│   └── plans/            # implementation plans (one per merged PR)
├── SYSTEM.md             # Otto's persona — copied to ~/.config/otto/system_prompt.md
├── TOTO.md               # Toto's persona (cat, busy-fallback + name-addressed chat)
├── TOOT.md               # Toot's persona (owl, release announcer + chat)
└── go.mod                # Go 1.26; deps: BurntSushi/toml, go-telegram-bot-api/v5,
                          # modernc.org/sqlite (pure-Go), modelcontextprotocol/go-sdk
```

## Memory system

Otto persists memory across messages, restarts, and `/new`. Two tiers (Hermes-style):

**Tier 1 — Curated core (always injected into every prompt).** Two bounded markdown files Otto can read directly as part of his system prompt:

- `~/.local/state/otto/memory/USER.md` — identity, preferences, communication style (cap ~500 tokens).
- `~/.local/state/otto/memory/MEMORY.md` — environment facts, project conventions, lessons (cap ~800 tokens).

Hand-editable. The `otto-memory` MCP server exposes `memory_add` / `memory_replace` / `memory_remove` so Claude can update them mid-conversation. Every write is security-scanned (credentials / prompt-injection patterns / invisible Unicode are rejected) and exact duplicates are blocked. At 80% of cap the next `memory_add` errors with the current contents, prompting consolidation.

**Tier 2 — Episodic + semantic store (on-demand, unbounded).** A SQLite database at `~/.local/state/otto/state.db`:

- `turns` table — every Otto/Toto/Toot exchange (user + assistant), append-only.
- `turns_fts` — FTS5 keyword mirror, kept in sync by trigger.
- `vectors` table — one embedding per turn (model + dim + blob).

Otto embeds each turn asynchronously after sending the reply (best-effort, 30 s-bounded, off the reply path). The `session_search` MCP tool embeds the query, runs semantic top-k + FTS5 in parallel, and merges (semantic first, keyword fills). If Ollama is unreachable or no model is pulled, search transparently degrades to FTS5 keyword only — nothing breaks.

**Semantic embeddings.** Local-only via Ollama at `http://localhost:11434`, ordered chain `embeddinggemma → nomic-embed-text → keyword floor`. Zero per-token cost, fully private. Vectors are tagged with `model` + `dim`, so a model swap silently ignores stale-dimension rows until they get re-embedded.

**Idle-gated session rotation.** Otto tracks `usage.input_tokens` per turn. When the session crosses **50 %** of the model's context window AND you've been quiet for **15 minutes**, a long-lived rotator goroutine claims Otto's slot and clears the session — your next message starts fresh, seeded by the always-injected memory core + retrieved memories. A **85 %** hard cap rotates at the next free moment regardless of idle. Continuity comes from the core (durable facts you've taught Otto) + `session_search` (any past turn). You should rarely need `/new` again.

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

- **`otto-memory`** — local Go binary at `~/.local/bin/otto-memory` (this repo). Tools: `memory_add`, `memory_replace`, `memory_remove`, `session_search`.
- `notion` — `@notionhq/notion-mcp-server` via `npx`.
- `google-calendar` — `@cocal/google-calendar-mcp` via `npx`.
- `gdrive` — `mcp-gdrive-workspace` via `npx`.
- `gmail-<label>` (one per Gmail account) — `@gongrzhe/server-gmail-autoauth-mcp` via `npx`.

Adding/removing servers: edit `mcp.json` and `systemctl --user restart otto`.

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
| `toto_persona_path` / `toto_session_id_path` | optional | from `TOTO.md` |
| `toot_persona_path` / `toot_session_id_path` | optional | from `TOOT.md` |
| `memory_dir` | `<session dir>/memory` | USER.md + MEMORY.md live here |
| `state_db_path` | `<session dir>/state.db` | turn log + vectors |
| `embed_ollama_url` | `http://localhost:11434` | local Ollama |
| `embed_models` | `["embeddinggemma","nomic-embed-text"]` | ordered fallthrough |
| `model_context_tokens` | `200000` | denominator for rotation thresholds |
| `rotate_soft_pct` | `0.50` | eligible to rotate once idle |
| `rotate_hard_pct` | `0.85` | rotate at next idle tick regardless |
| `rotate_idle_minutes` | `15` | quiet window required for a soft rotation |

## Troubleshooting

- **`go build` fails:** check `go.mod`'s `go` directive matches your installed Go (`go version`).
- **Otto not starting:** `journalctl --user -u otto -n 100`. Common causes: missing `~/.config/otto/client_secret.json`, missing `claude` on PATH (the unit's `Environment=PATH=` covers `~/.local/bin` and standard locations).
- **Telegram messages not arriving:** check the bot token in `config.toml`, and that `telegram_allowed_user_id` matches the user you're texting from. Non-allowlisted users are silently dropped.
- **Google auth expired:** re-run `setup.sh`; it will re-prompt for whichever credential is missing.
- **Memory not persisting facts:** confirm `otto-memory` is in `mcp.json` (`grep otto-memory ~/.config/otto/mcp.json`) and that `~/.local/state/otto/memory/` is writable. Logs print `turn log` / `embed turn` errors at the `otto` journal.
- **Semantic search not working:** check Ollama (`systemctl --user status ollama` on Linux, `brew services list` on macOS) and `ollama list`. Without a pulled embedding model, `session_search` falls back to keyword (FTS5) and logs `session_search: embed unavailable, keyword-only`. To enable semantic recall after a fresh install: `ollama pull embeddinggemma`.
- **Session never rotates:** the rotator only fires when (a) the tracked `input_tokens` from Claude's `result` events crosses the soft threshold, (b) you've been idle ≥ `rotate_idle_minutes`, and (c) Otto isn't busy. The journal logs `rotator: rotated session ...` on success. Disable by setting `model_context_tokens = 0`.
- **Claude `@<path>` image syntax wrong:** if images don't work, check `internal/claude/runner.go` and adjust against the installed Claude Code version's CLI.
- **`/update` seems to hang:** after `/update`, the bot exits within ~10s and systemd brings up the new binary on the next tick. Toot pings you back from the fresh process once it's settled. If you don't see that ping within ~30s, check `systemctl --user status otto` and `journalctl --user -u otto -n 50`.

## License

Private / personal use.
