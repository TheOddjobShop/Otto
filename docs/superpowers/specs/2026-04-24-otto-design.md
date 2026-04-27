# Otto — Telegram bot for Claude Code

**Status:** design  
**Date:** 2026-04-24  
**Owner:** single user (home server deployment)

## Goal

A single-user personal-assistant bot. The user texts it on Telegram; it relays each message to Claude Code running on the user's Arch Linux home server, which has tool access to Notion, Gmail, Google Drive, Google Docs, and Google Calendar via MCP servers. Conversation memory persists across messages and across restarts. Runs forever as a systemd user service.

## Non-goals (explicit YAGNI)

- Voice messages (no Whisper transcription).
- Multi-user / per-user sessions. Allowlist contains exactly one Telegram user ID.
- Web UI or status dashboard. Use `journalctl --user -u otto`.
- Auto-update / self-upgrade. Re-run `setup.sh` to upgrade.
- Encryption-at-rest for tokens. Filesystem perms `0600` only — single-user home server, local disk trusted.
- Webhook mode for Telegram. Long-polling only (no public IP / TLS cert needed).

## Architecture

Two Go binaries plus one third-party MCP server:

- **`otto`** — Telegram daemon. Long-polls Telegram, validates the sender against the allowlist, downloads any attached photos, and shells out to `claude -p ... --resume <session-id>` for each message. Streams the response back to Telegram, chunking at the 4096-char limit.
- **`google-mcp`** — Custom Go MCP server (stdio transport). Exposes Gmail, Drive, Docs, and Calendar tools to Claude Code using Google's official Go SDKs. One refresh token, one process, lives in the same repo.
- **`@notionhq/notion-mcp-server`** — Official npm Notion MCP server, launched by `claude` via `npx -y` per `mcp.json`.

The Go side stays small and reliable; Claude Code does the heavy lifting (planning, tool use loop, conversation state). MCP servers are config-driven — adding/removing one is a `mcp.json` edit.

### Why a custom Google MCP server (not community packages)

The community Google MCP ecosystem is split across multiple packages of uneven quality, often one per API. Wiring four of them is brittle: four token-refresh paths, four sets of failure modes, four upgrade cadences. A small Go MCP server using the official Google SDKs gives one cohesive piece of code, one refresh token, one process to monitor — and matches the project's "Go application" framing.

## Repository layout

```
.
├── cmd/
│   ├── otto/                  # Telegram daemon entrypoint
│   └── google-mcp/            # Custom Google MCP server entrypoint
├── internal/
│   ├── telegram/              # Long-polling, send, image download, chunking
│   ├── claude/                # CLI subprocess + session ID persistence
│   ├── google/                # OAuth flow + Gmail/Drive/Docs/Calendar clients
│   ├── auth/                  # Telegram allowlist check
│   └── config/                # config.toml loader
├── systemd/
│   └── otto.service           # Template for systemd user unit
├── docs/superpowers/specs/    # This spec lives here
├── setup.sh                   # One-shot installer
├── go.mod
└── README.md
```

## Per-message data flow

1. Telegram long-poll returns an update.
2. Allowlist check — drop anything that isn't the configured user ID. Silent drop, no reply (do not leak the bot's existence).
3. If the message contains photos: download each via the Telegram Bot API to a temp file, base64-encode, and reference them in the Claude prompt using whatever image-passing mechanism Claude Code's CLI provides.
4. Spawn: `claude -p "<text>" --resume <session-id> --mcp-config ~/.config/otto/mcp.json --output-format stream-json`.
5. Read stream-json events, accumulate assistant text, forward to Telegram. Split responses >4096 chars at paragraph boundaries into multiple messages.
6. On error: send `⚠️ <error>` back to the user and log to journal.

## Session memory

One session ID per install, stored at `~/.local/state/otto/session_id`. Every `claude` call uses `--resume <id>`. On bot start, the session ID is read from disk; if absent, a UUID is generated and persisted.

The `/new` bot command rotates the session ID, starting a fresh conversation. Old session histories are retained by Claude Code internally — `/new` only affects which one Otto resumes.

## Concurrency

One Claude Code call at a time per session, FIFO-queued. If a message arrives while a call is in-flight, it queues. Sequencing matters because parallel `--resume` against the same session ID would race on session history.

## Claude Code authentication

Otto delegates Anthropic auth to Claude Code itself. Each `claude` subprocess inherits Otto's process environment unchanged, so whatever Claude Code is already configured with — `claude /login` (browser-based, stores under `~/.claude/`), `claude setup-token` (long-lived token, good for headless), or an `ANTHROPIC_API_KEY` set in the parent's environment — is what the subprocess uses.

Otto does not store, prompt for, or manage Anthropic credentials. The `setup.sh` script verifies that `~/.claude/` exists or `ANTHROPIC_API_KEY` is set in the running shell before proceeding; if neither is present it instructs the user to run `claude /login` (or equivalent) and re-run setup.

For the `systemd` user service, `~/.claude/` lives under the user's home (auto-inherited via `%h`); if the user prefers the env-var path, they add `Environment=ANTHROPIC_API_KEY=...` to the unit's `[Service]` section.

## MCP server configuration

`~/.config/otto/mcp.json` (written by `setup.sh`):

```json
{
  "mcpServers": {
    "notion": {
      "command": "npx",
      "args": ["-y", "@notionhq/notion-mcp-server"],
      "env": { "NOTION_API_KEY": "secret_..." }
    },
    "google": {
      "command": "/home/<user>/.local/bin/google-mcp",
      "env": {
        "GOOGLE_TOKEN_PATH": "/home/<user>/.config/otto/google_token.json",
        "GOOGLE_CLIENT_SECRET_PATH": "/home/<user>/.config/otto/client_secret.json"
      }
    }
  }
}
```

## google-mcp tool surface (initial)

Minimal but useful. Expand later as needs surface.

| API | Tools |
|---|---|
| Gmail | `gmail_search`, `gmail_read`, `gmail_send`, `gmail_modify_labels` |
| Drive | `drive_search`, `drive_read_file`, `drive_upload`, `drive_create_folder` |
| Docs | `docs_read`, `docs_create`, `docs_append` |
| Calendar | `calendar_list_events`, `calendar_create_event`, `calendar_find_free_time` |

Each tool is a thin wrapper over the Google Go SDK. Token refresh is automatic via `oauth2.TokenSource`. On a hard refresh failure (refresh token revoked), the tool returns a structured error so Claude can surface it.

## setup.sh

Idempotent, safe to re-run. Phases:

1. **Preflight.** Verify Arch + pacman, that the script is not run as root, that `~/.config` exists.
2. **System deps.** `sudo pacman -S --needed go nodejs npm jq curl base-devel`.
3. **Claude Code.** `sudo npm i -g @anthropic-ai/claude-code`. Verify with `claude --version`.
4. **Build Go binaries.** `go build -o ~/.local/bin/otto ./cmd/otto && go build -o ~/.local/bin/google-mcp ./cmd/google-mcp`.
5. **Prompt for secrets.** Skip any already present in `~/.config/otto/config.toml`:
   - Telegram bot token (from @BotFather).
   - Owner Telegram user ID (instruct user to message @userinfobot).
   - Anthropic API key.
   - Notion integration token (from notion.so/profile/integrations).
6. **Google Cloud Console manual step.** Print exact click-by-click instructions: create GCP project → enable Gmail/Drive/Docs/Calendar APIs → create OAuth consent screen (External, Testing mode, add self as test user) → create OAuth Desktop client → download `client_secret.json` → drop at `~/.config/otto/client_secret.json`. Wait for "Press Enter when done."
7. **OAuth consent flow.** Run `otto --google-auth`. This spins up a localhost callback server, prints a URL, the user clicks through Google's consent screen once, refresh token is saved to `~/.config/otto/google_token.json`.
8. **Write `mcp.json`** at `~/.config/otto/mcp.json` with absolute paths and the Notion token.
9. **systemd user service.** Write `~/.config/systemd/user/otto.service` from the repo template, run `loginctl enable-linger $USER` (so the service runs after logout), then `systemctl --user enable --now otto`.
10. **Smoke test.** `systemctl --user status otto` and tail journal for 5s, print `✅ Send a message to your bot.`

## File layout on disk

| Path | Contents | Permissions |
|---|---|---|
| `~/.config/otto/config.toml` | Telegram token, allowed user ID, Anthropic API key, Notion token | `0600` |
| `~/.config/otto/client_secret.json` | OAuth client (user-supplied) | `0600` |
| `~/.config/otto/google_token.json` | OAuth refresh token | `0600` |
| `~/.config/otto/mcp.json` | MCP server config | `0600` |
| `~/.local/state/otto/session_id` | Current Claude session UUID | `0600` |
| `~/.local/bin/otto`, `~/.local/bin/google-mcp` | Binaries | `0755` |
| `~/.config/systemd/user/otto.service` | Service unit | `0644` |

## systemd unit

```ini
[Unit]
Description=Otto — Telegram bot for Claude Code
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/otto
Restart=always
RestartSec=5
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=default.target
```

`loginctl enable-linger $USER` keeps it running after logout. Logs: `journalctl --user -u otto -f`.

## Bot commands

All start with `/`:

- `/new` — rotate session ID, start a fresh Claude conversation.
- `/whoami` — print the sender's Telegram ID and current session ID (debug).
- `/restart` — kill any in-flight `claude` subprocess (use if hung).
- `/status` — uptime, session ID, last error if any.

## Error handling

| Failure | Response |
|---|---|
| Telegram API hiccup (network, 5xx) | Exponential backoff (1s → 60s cap), keep polling. |
| Non-allowlisted user message | Silent drop, debug-level log. |
| Claude Code subprocess crash | Catch exit code, send `⚠️ Claude error: <stderr last line>`, continue. |
| Claude Code hangs | 5-minute hard timeout per message; kill process group; send `⚠️ Timeout — try /restart`. |
| MCP server crash | Surfaces as a tool error in the Claude stream (per-message lifecycle). |
| OAuth refresh token revoked | google-mcp returns structured error; otto surfaces `⚠️ Google auth expired — re-run setup.sh`. |
| Telegram message >4096 chars | Auto-chunk on paragraph boundaries, send sequentially. |
| Otto panic | systemd restarts after 5s. |

## Testing strategy

- **Unit tests.** `internal/telegram` chunking; `internal/auth` allowlist; `internal/google` token refresh; each `cmd/google-mcp` tool with `httptest`-mocked Google APIs.
- **Integration tests.** `make test-integration` brings up otto with a fake Telegram server (`httptest`) plus recorded Google API responses, sends a scripted conversation, asserts on outputs. Skipped in CI without `INTEGRATION=1`.
- **Manual smoke test** documented in README: send "hi" and "what's on my calendar today?" after setup.

## Known implementation questions (resolved during plan/build)

- Exact CLI invocation for image attachments. Claude Code supports images, but the precise flag/syntax (e.g., `@/path/to/image.jpg`, multimodal stdin, etc.) needs to be verified at implementation time and may shape `internal/telegram`'s temp-file lifecycle.
- Default Anthropic model. Inherit Claude Code's default unless we have reason to pin (e.g., context-window or cost considerations).
- Stream-json event schema for partial messages. Plan should pin a specific Claude Code version and verify the parser against its actual output.
