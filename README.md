# Otto

Single-user Telegram bot wrapping Claude Code with MCP tools (Notion + Gmail + Google Drive + Google Calendar). Runs perpetually as a `systemd --user` service on an Arch Linux home server. Persistent conversation memory across messages and restarts.

Design spec: [`docs/superpowers/specs/2026-04-24-otto-design.md`](docs/superpowers/specs/2026-04-24-otto-design.md)

## Quick start

```bash
./setup.sh
```

The script is idempotent — re-run anytime to add credentials or fix things; it skips what's already done. It detects Arch Linux (`pacman`) or macOS (`brew`) and adapts; the systemd unit is Linux-only. It will:

1. Install system deps (`go`, `nodejs`, `npm`, `jq`, `curl`, `python`, `lsof`, plus `base-devel` on Arch).
2. Install Claude Code CLI globally via `npm`.
3. Build the `otto` binary into `~/.local/bin/`.
4. Walk through one-time Google Cloud Console setup (OAuth Desktop client).
5. Browser sign-ins for Google Calendar, Drive, Gmail.
6. Prompt for Notion integration token, Telegram bot token + your Telegram user ID. (Anthropic auth is delegated to Claude Code — see "Claude Code authentication" below.)
7. Write `~/.config/otto/{config.toml, mcp.json, client_secret.json}` with `0600` perms.
8. On Linux, install a `systemd --user` service at `~/.config/systemd/user/otto.service`, enable lingering, start the service, and tail the journal briefly to confirm it's healthy.

## Manual smoke test

After `setup.sh` reports success, on Telegram:

- Send `hi` — Otto replies.
- Send `My name is Alice.` then `What's my name?` — second reply should know "Alice" (persistent session).
- Send `/new` then `What's my name?` — should not remember.
- Send `/whoami` — prints your Telegram user ID and current session ID.
- Send `/status` — prints uptime + session.
- Send `/restart` — acks the in-flight call (no-op; messages already serialize).
- Send a photo with caption "describe this" — Otto downloads it and forwards to Claude via `@<path>`.
- Send "what's on my calendar today?" — exercises the Google Calendar MCP.

## Operations

```bash
journalctl --user -u otto -f          # tail logs
systemctl --user status otto          # check status
systemctl --user restart otto         # restart
systemctl --user stop otto            # stop
```

## Build / test

```bash
make build              # builds ./otto
make test               # unit tests for all packages
make test-integration   # integration tests (uses testdata/fake-claude.sh as a stub claude)
make vet                # go vet + gofmt check
go test -race ./...
```

## Layout

```
.
├── cmd/otto/             # daemon entrypoint, message handler, bot commands, markdown stripping
├── internal/
│   ├── config/           # TOML config loader
│   ├── auth/             # single-user allowlist
│   ├── telegram/         # Bot API wrapper, chunking, image download, inline keyboards
│   ├── claude/           # subprocess Runner + stream-json parser + session ID persistence
│   └── permissions/      # pending tool-denial decisions + settings.json writer
├── systemd/otto.service  # user-service template (Linux only)
├── setup.sh              # idempotent installer (Arch + macOS)
├── testdata/             # fake-claude.sh stub for integration tests
├── docs/superpowers/
│   ├── specs/            # design specs
│   └── plans/            # implementation plans
├── SYSTEM.md             # default contents for ~/.config/otto/system_prompt.md
└── go.mod                # Go 1.26.2; deps: BurntSushi/toml, go-telegram-bot-api/v5
```

## Tool permissions

Otto invokes `claude` with `--dangerously-skip-permissions`. Claude Code's normal interactive permission prompt has no surface in `-p` mode, so it would otherwise reject every MCP tool call (`mcp__gmail__*`, `mcp__gdrive__*`, etc.) and every `Bash`/`Write` invocation, making the bot useless. The flag bypasses that gate entirely.

The threat model that makes this acceptable: Otto is a single-user bot on your own server, every incoming message is checked against `telegram_allowed_user_id` before reaching Claude, and you're texting your own bot from your own phone. Anyone with bot-token + allowlisted-user-ID could in theory spoof Telegram, but that's a different security perimeter — Otto's gate is at the Telegram allowlist, not at Claude's per-tool prompt.

When a tool call still slips through and is denied (Claude returns `permission_denials` on its result event), Otto surfaces it as a Telegram message with three inline-keyboard buttons:

- **Allow once** — replays the original prompt with `--allowed-tools <pattern>`; no persistent change.
- **Allow always** — writes the pattern (e.g. `mcp__gmail-personal__*`) into `~/.claude/settings.json`'s `permissions.allow` array, then replays.
- **Deny** — silent ack.

Pending decisions are kept in memory (capped, GC'd by age) keyed by a short opaque ID embedded in the callback data, so a tap auto-replays without re-sending. Image attachments aren't preserved across replays — re-send the photo if you need it.

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

`setup.sh` writes `~/.config/otto/mcp.json` referencing four community MCP servers, all run via `npx`:

- `@notionhq/notion-mcp-server`
- `@cocal/google-calendar-mcp`
- `@gongrzhe/server-gmail-autoauth-mcp`
- `mcp-gdrive-workspace`

Adding/removing servers: edit `mcp.json` and `systemctl --user restart otto`.

## Troubleshooting

- **`go build` fails:** check `go.mod`'s `go` directive matches your installed Go (`go version`).
- **Otto not starting:** `journalctl --user -u otto -n 100` shows the last 100 lines. Common causes: missing or zero-byte `~/.config/otto/client_secret.json`, missing `claude` on PATH (the unit's `Environment=PATH=` covers `~/.local/bin` and standard locations).
- **Telegram messages not arriving:** check the bot token in `config.toml`, and that `telegram_allowed_user_id` matches the user you're texting from. Non-allowlisted users are silently dropped.
- **Google auth expired:** re-run `setup.sh`; it will re-prompt for whichever credential is missing.
- **Claude `@<path>` image syntax wrong:** verified during `setup.sh` smoke test. If images don't work, check `internal/claude/runner.go` and adjust against the installed Claude Code version's CLI.

## License

Private / personal use.
