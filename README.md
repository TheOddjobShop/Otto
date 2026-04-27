# Otto

Single-user Telegram bot wrapping Claude Code with MCP tools (Notion + Gmail + Google Drive + Google Calendar). Runs perpetually as a `systemd --user` service on an Arch Linux home server. Persistent conversation memory across messages and restarts.

Design spec: [`docs/superpowers/specs/2026-04-24-otto-design.md`](docs/superpowers/specs/2026-04-24-otto-design.md)

## Quick start

```bash
./setup.sh
```

The script is idempotent — re-run anytime to add credentials or fix things; it skips what's already done. It will:

1. Install system deps via `pacman` (`go`, `nodejs`, `npm`, `jq`, `curl`, `python`, `lsof`, `base-devel`).
2. Install Claude Code CLI globally via `npm`.
3. Build the `otto` binary into `~/.local/bin/`.
4. Walk through one-time Google Cloud Console setup (OAuth Desktop client).
5. Browser sign-ins for Google Calendar, Drive, Gmail.
6. Prompt for Notion integration token, Telegram bot token + your Telegram user ID. (Anthropic auth is delegated to Claude Code — see "Claude Code authentication" below.)
7. Write `~/.config/otto/{config.toml, mcp.json, client_secret.json}` with `0600` perms.
8. Install a `systemd --user` service at `~/.config/systemd/user/otto.service`, enable lingering, start the service, and tail the journal briefly to confirm it's healthy.

## Manual smoke test

After `setup.sh` reports success, on Telegram:

- Send `hi` — Otto replies.
- Send `My name is Alice.` then `What's my name?` — second reply should know "Alice" (persistent session).
- Send `/new` then `What's my name?` — should not remember.
- Send `/whoami` — prints your Telegram user ID and current session ID.
- Send `/status` — prints uptime + session.
- Send a photo with caption "describe this" — Otto downloads it and forwards to Claude.
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
make build       # builds ./otto
make test        # unit tests for all internal packages
go test -race ./...
```

## Layout

```
.
├── cmd/otto/             # daemon entrypoint, message handler, bot commands
├── internal/
│   ├── config/           # TOML config loader
│   ├── auth/             # single-user allowlist
│   ├── telegram/         # Bot API wrapper, chunking, image download
│   └── claude/           # subprocess Runner + stream-json parser + session ID
├── systemd/otto.service  # user-service template
├── setup.sh
├── docs/superpowers/
│   ├── specs/            # design specs
│   └── plans/            # implementation plans
└── go.mod
```

## Tool permissions

Otto invokes `claude` with `--dangerously-skip-permissions`. Claude Code's normal interactive permission prompt has no surface in `-p` mode, so it would otherwise reject every MCP tool call (`mcp__gmail__*`, `mcp__gdrive__*`, etc.) and every `Bash`/`Write` invocation, making the bot useless. The flag bypasses that gate entirely.

The threat model that makes this acceptable: Otto is a single-user bot on your own server, every incoming message is checked against `telegram_allowed_user_id` before reaching Claude, and you're texting your own bot from your own phone. Anyone with bot-token + allowlisted-user-ID could in theory spoof Telegram, but that's a different security perimeter — Otto's gate is at the Telegram allowlist, not at Claude's per-tool prompt.

If you want stricter behavior (e.g., require Telegram inline-keyboard approval per tool call), that's a future feature — for now, every tool call goes through.

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
