# Otto: remove inline buttons + add opt-in self-update

Date: 2026-05-03
Repo: https://github.com/TheOddjobShop/Otto

## Context

Otto is a single-user Telegram bot wrapping Claude Code. Two changes:

1. **Remove the inline-keyboard permission flow.** The Once / Always / Skip buttons surfaced after a tool denial feel unresponsive in practice — the "Replaying…" toast dismisses immediately, then Otto spends 15-60s generating the replay reply, so from the user's side it looks like nothing happened. Combined with the rarity of denials under `--dangerously-skip-permissions`, the UX cost outweighs the value.
2. **Add opt-in self-update.** Otto is going semi-public. Users who installed via `setup.sh` won't have a git checkout, so updating means manual rebuild today. We want Otto to notify the user when a new release exists and apply it on `/update`.

The two changes are bundled because removing inline buttons sets the UX direction (slash-command-only) for the new feature.

## Part A: remove inline-keyboard infrastructure

### Scope of removal

Delete:

- **`internal/permissions/`** — entire package. Includes `Pending`, `Entry`, `AllowTool`, `PatternFor`, and tests.
- **`cmd/otto/handler.go`**:
  - `pending` and `settingsPath` fields on `handler`
  - `handleCallback` method
  - `surfaceDenials` method
  - `IsCallback()` branch in `dispatch`
  - `callbackPrefixPerm` constant
- **`cmd/otto/main.go`**:
  - `permissions` import
  - `permissions.New(64)` initialization
  - `settingsPath` resolution
- **`internal/telegram/client.go`**:
  - `SendMessageWithButtons` and `AnswerCallbackQuery` from the `BotClient` interface and `realClient`
  - `InlineButton` type
  - `CallbackQueryID`, `CallbackData`, `CallbackMessageID` fields on `Update`
  - `IsCallback()` method
  - The `if u.CallbackQuery != nil` branch in `fromTGUpdate`
- **`cmd/otto/tty.go`**: `SendMessageWithButtons` and `AnswerCallbackQuery` stubs
- **`internal/config/config.go`**: `ClaudeSettingsPath` field (now unused)

### New permission-denial behavior

In `runAndReply`, when `lastResult.PermissionDenials` is non-empty, send one Telegram message per unique tool with copy-pasteable instructions:

> ⚠️ Claude tried to use `mcp__gmail-personal__send_message` and was denied.
>
> To allow it next time, add `mcp__gmail-personal__*` to `permissions.allow` in `~/.claude/settings.json`, then `/restart`.

The wildcard pattern derivation logic (currently `permissions.PatternFor`) moves inline into `handler.go` as a small private helper — it's six lines and only one caller.

### Migration

- Existing users keep their `~/.claude/settings.json` rules unchanged. Claude Code reads them natively; Otto just no longer writes to that file.
- Existing `config.toml` files with `claude_settings_path = "..."` are silently ignored after removal (BurntSushi/toml drops unknown fields by default). No breakage.
- Update `setup.sh` to stop writing the `claude_settings_path` line in newly generated configs. Existing configs are untouched.

## Part B: opt-in self-update

### High-level flow

1. CI publishes a GitHub Release with cross-compiled binaries when a `v*` tag is pushed.
2. Otto polls `releases/latest` once an hour. If the latest tag differs from its own embedded version, it sends one Telegram message: *"Update available: v1.2.3 → v1.2.4. Reply `/update` to install."*
3. User types `/update` whenever they want. Otto downloads the platform-matched binary, atomically swaps it in, sends a confirmation, and exits cleanly. systemd's `Restart=always` brings Otto back on the new binary.

### Build / release pipeline

**File:** `.github/workflows/release.yml`

**Trigger:** push of a tag matching `v*` (e.g. `v1.2.3`).

**Steps:**

1. Checkout
2. Setup Go (1.26.x, matching `go.mod`)
3. Build matrix — three targets:
   - `linux/amd64` → `otto-linux-amd64`
   - `linux/arm64` → `otto-linux-arm64`
   - `darwin/arm64` → `otto-darwin-arm64`
4. For each target: `GOOS=$os GOARCH=$arch go build -ldflags "-X main.version=${{ github.ref_name }}" -o otto-$os-$arch ./cmd/otto`
5. Create a GitHub Release for the tag and attach all three binaries (e.g. via `softprops/action-gh-release@v2`)

Releases are public artifacts on a public repo. No signing or checksum verification in v1; the trust anchor is GitHub HTTPS + the user's GitHub account 2FA. Adding SHA256 manifests / sigstore can come later.

### Version constant

Add to `cmd/otto/main.go`:

```go
var version = "dev"
```

Set at build time via `-ldflags "-X main.version=v1.2.3"`. Update the `Makefile`'s `build` and `install` targets to accept an optional `VERSION` env var:

```make
VERSION ?= dev
build:
	go build -ldflags "-X main.version=$(VERSION)" -o ./otto ./cmd/otto
```

Local development builds get `version = "dev"` and skip update polling entirely. CI sets `VERSION` from the tag name.

### Update poller

**New file:** `cmd/otto/updater.go`

**Constants:**
- `updateCheckInterval = 1 * time.Hour`
- `updateInitialDelay = 30 * time.Second` (don't hammer GitHub on every restart)
- `releasesURL = "https://api.github.com/repos/TheOddjobShop/Otto/releases/latest"`

**Goroutine signature:** `func runUpdatePoller(ctx context.Context, h *handler, chatID int64)` — started from `main.go` after handler is constructed. The single allowlisted user ID is the notification target.

**Behavior per tick:**

1. Skip the whole loop if `version == "dev"`.
2. `GET releasesURL` (no auth; 60 req/hr unauthenticated rate limit is plenty for hourly polling).
3. Parse the JSON for `tag_name` and `assets[]` (each asset has `name` and `browser_download_url`).
4. If `tag_name == version`, no-op.
5. If `tag_name == lastAnnounced` (in-memory state on the updater struct), no-op — already told the user about this version.
6. Otherwise: send Telegram message `"Update available: <version> → <tag_name>. Reply /update to install."`, store the asset URL keyed by `runtime.GOOS`/`runtime.GOARCH` in a thread-safe `pendingUpdate` field on the handler, set `lastAnnounced = tag_name`.

**Concurrency:** the `pendingUpdate` field is a struct holding `{tag string, assets map[string]string}` guarded by its own mutex. Read by `/update` from the dispatch goroutine; written by the poller goroutine. Standard mutex.

### `/update` command

Lives in `commands.go`. Does not acquire the Otto slot — runs even when Otto is mid-task. If Otto is busy, the eventual SIGTERM will cancel the in-flight call (the user's work is lost). The synchronous reply explicitly warns about this so the user can wait if needed.

**Synchronous reply** (returned from `tryCommand`):

- No pending update → *"No update available. You're on v1.2.3."*
- Pending update, no asset for platform → *"Update v1.2.4 is available, but no binary for linux/arm64. Skip or build manually."*
- Pending update, asset present, Otto idle → *"Starting update to v1.2.4 for linux/amd64…"*
- Pending update, asset present, Otto busy → *"Starting update to v1.2.4 for linux/amd64. (Otto is currently working on `<prompt>` — that work will be interrupted.)"*

**Side-effect goroutine** (spawned only when an asset is present, before `tryCommand` returns):

1. Download the asset URL with a 5-minute timeout. Read into memory (binaries are ~10MB; OK to hold).
2. Sanity check: bytes are non-empty.
3. Resolve own binary path: `os.Executable()` → `filepath.EvalSymlinks`.
4. Write bytes to `<path>.new` with mode `0755`.
5. `os.Rename(<path>.new, <path>)` — atomic on the same filesystem (true for `~/.local/bin/`).
6. Send Telegram message: *"Installed v1.2.4. Restarting…"* (`bot.SendMessage` is synchronous — by the time it returns nil, Telegram has the message; no extra delay needed.)
7. `syscall.Kill(os.Getpid(), syscall.SIGTERM)` — triggers the existing signal handler in `main.go`, which calls `cancel()`. The polling loop exits, `h.WaitDispatches()` drains in-flight calls (cancelling them), the process exits, systemd restarts on the new binary.

If any step fails: send a Telegram message describing the failure (`"Download failed: <err>. Try again later."`, `"Can't write to /usr/local/bin/otto: permission denied. Update manually."`, etc.) and abort. Do not exit.

### `/version` command

Trivial. Replies with `"version=v1.2.3 build=<go-version> os=<goos>/<goarch>"`. Useful for debugging "did the update actually land."

### Platform support

- **Linux (systemd user service):** full flow works. Restart is automatic.
- **macOS (manual `otto` invocation):** download + swap works, but Otto can't relaunch itself. After exit the user re-runs `otto`. Document this; don't add a separate code path.
- **Other (Windows, BSD, etc.):** not supported. The poller will simply find no matching asset and report the platform-mismatch message.

### State

In-memory only:

- `version` — compile-time constant, never changes
- `pendingUpdate` — last detected available release, cleared on Otto restart
- `lastAnnounced` (inside the updater) — most recent tag we sent a notification for

If Otto restarts (whether for an update or any other reason), the next poller tick re-detects whatever's available and re-announces. This is fine because restarts are infrequent — we don't get a notification spam loop.

No on-disk state for the updater. No version history file. The user can always check `/version` to see what's running.

## Out of scope

- Signed releases / SHA256 manifest verification (can be added later without breaking the API)
- Fully automatic install without `/update` confirmation
- Semver parsing or downgrade detection (`tag != version` is sufficient)
- Background pre-download of the binary (download only when `/update` is invoked, no partial-state on disk)
- Rollback to a previous version (user re-installs manually if needed)
- Auto-update of `mcp.json`, `system_prompt.md`, or any user-facing config (only the binary)
- Notifying multiple users (Otto is single-user by design; the allowlisted user ID is the notification target)

## Testing

- **Permissions removal:** existing tests in `internal/permissions/` and the callback paths get deleted along with the code. Verify `go test ./...` and `go vet ./...` pass after removal.
- **Updater logic:**
  - Mock the GitHub API with `httptest.NewServer` returning canned `releases/latest` JSON.
  - Test version comparison (`tag == version`, `tag != version`, `tag == lastAnnounced`).
  - Test asset matching (`assetForPlatform(assets, "linux", "amd64")` finds `otto-linux-amd64`, returns `("", false)` for absent platforms).
  - Test the notification deduplication (two ticks with the same tag → one message).
- **`/update` command:**
  - Fake the download via an injected HTTP client; write to a temp dir as the "binary."
  - Verify the swap happens, the confirmation message is sent.
  - Don't actually SIGTERM in tests — inject the exit function as a closure so tests can observe it was called.
- **Integration:** add a test that runs the full updater→`/update` flow against a `httptest` server with a real binary file as the asset.

## Open questions

None — design is fully specified. Implementation can begin once this spec is approved.
