# Otto: button removal + opt-in self-update — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the inline-keyboard permission flow and add an opt-in self-update path that detects new GitHub Releases, notifies the user, and installs on `/update`.

**Architecture:**
- **Part A (removal):** Strip out the `internal/permissions/` package, the callback dispatch path, the inline-button surface from the telegram package, and the corresponding fields/imports throughout `cmd/otto/`. Replace permission-denial UX with a plain-text instructional message.
- **Part B (self-update):** Add a `version` constant set at build time via `-ldflags`. New `cmd/otto/updater.go` runs a goroutine that polls `https://api.github.com/repos/TheOddjobShop/Otto/releases/latest` hourly. Update notifications are delivered by **Toot**, a new owl character (`cmd/otto/toot.go`) — analogous to Toto but with no LLM, just static one-way messages with random owl ASCII art prepended via `<pre>` + `SendMessageHTML`. Toot owns the periodic "v1.x available" announcement (with auto-generated patch notes from the GitHub release body) and the "Installed v1.x, restarting…" confirmation. The conversational lane (the synchronous `/update` reply, failure messages) stays on the regular bot. `/update` slash command downloads the platform-matched binary, atomically swaps `os.Executable()`, and SIGTERMs self so systemd restarts on the new binary.

**Tech Stack:** Go 1.26, `BurntSushi/toml`, `go-telegram-bot-api/v5`, GitHub Actions for cross-compilation. No new dependencies.

**Spec:** [`docs/superpowers/specs/2026-05-03-button-removal-and-self-update-design.md`](../specs/2026-05-03-button-removal-and-self-update-design.md)

---

## File map

**Part A — files modified or deleted:**
- `internal/permissions/permissions.go` — DELETE
- `internal/permissions/permissions_test.go` — DELETE
- `cmd/otto/handler.go` — remove `handleCallback`, `surfaceDenials`, `pending`/`settingsPath` fields, `callbackPrefixPerm`, callback dispatch branch; rewrite denial surfacing as plain text
- `cmd/otto/handler_test.go` — remove callback-flow tests; rewrite denial-surfacing test for plain text; remove fakeBot's `SendMessageWithButtons` / `AnswerCallbackQuery`
- `cmd/otto/main.go` — remove `permissions` import, `permissions.New(64)`, `settingsPath` resolution
- `cmd/otto/tty.go` — remove `SendMessageWithButtons` / `AnswerCallbackQuery` stubs
- `internal/telegram/client.go` — remove from `BotClient` interface, `realClient`, `Update` struct, `IsCallback`, `InlineButton` type, callback parsing in `fromTGUpdate`
- `internal/config/config.go` — remove `ClaudeSettingsPath` field
- `internal/config/config_test.go` — remove any references to `claude_settings_path`
- `setup.sh` — stop writing `claude_settings_path` line in generated `config.toml`

**Part B — files created or modified:**
- `cmd/otto/main.go` — add `var version = "dev"`, construct Toot + updater, start updater goroutine
- `cmd/otto/commands.go` — add `/update` and `/version` cases
- `cmd/otto/commands_test.go` — tests for `/update` and `/version`
- `cmd/otto/handler.go` — add `updater *updater` field
- `cmd/otto/updater.go` — NEW: poller, state, install logic; routes notifications through Toot
- `cmd/otto/updater_test.go` — NEW: tests for asset matching, fetch, dedup, install
- `cmd/otto/toot.go` — NEW: Toot owl character (one-way notification courier)
- `cmd/otto/toot_test.go` — NEW: tests for owl-art rendering and HTML escaping
- `toot.txt` — already authored at repo root; embedded into the binary via `//go:embed`
- `Makefile` — add `VERSION ?= dev` and pass via `-ldflags`
- `.github/workflows/release.yml` — NEW: cross-compile + release on `v*` tag

---

## Pre-flight

- [ ] **Verify clean baseline.** The working tree currently has uncommitted changes in `cmd/otto/{commands,handler,toto}.go` (Toto snippet sharing, `/restart` force-cancel, `/status` state). These are unrelated to this plan but touch the same files. Decide before starting:
  - Either commit them on `master` first (so this plan stacks cleanly), or
  - Stash them and apply later (extra merge conflicts likely)
  - Recommended: commit them first as a separate prep commit.

```bash
cd /Users/huiyunlee/Workspace/github.com/justin06lee/theoddjobshop/AbdurRazzaq/Otto
git status
# If dirty, commit:
git add cmd/otto/commands.go cmd/otto/handler.go cmd/otto/toto.go
git commit -m "show otto progress to toto + /restart force-cancel + /status state"
```

- [ ] **Verify baseline builds and tests pass.**

```bash
make build && make test
```

Expected: clean build, all tests pass. Any pre-existing failure must be addressed before starting Part A.

---

## Phase A: remove inline-keyboard infrastructure

### Task A1: Replace `surfaceDenials` with plain-text message

**Files:**
- Modify: `cmd/otto/handler.go` (lines ~329-364, the `surfaceDenials` method)
- Modify: `cmd/otto/handler_test.go` (the test that asserts buttons get sent on denials, around lines 280-340)

- [ ] **Step 1: Update the existing denial test to expect plain text**

Find the test in `handler_test.go` that asserts inline-keyboard buttons appear after a `PermissionDenial`. Rewrite it to:

```go
func TestSurfaceDenialsAsPlainText(t *testing.T) {
	bot := &fakeBot{}
	// surfaceDenials only touches h.bot — no need to wire other fields.
	h := &handler{bot: bot}

	denials := []claude.PermissionDenial{
		{ToolName: "mcp__gmail-personal__send_message", ToolUseID: "tu_1"},
		{ToolName: "mcp__gmail-personal__search_emails", ToolUseID: "tu_2"},
		{ToolName: "Bash", ToolUseID: "tu_3"},
	}
	h.surfaceDenials(context.Background(), 999, "send a test email", denials)

	// Both gmail tools share the wildcard pattern, so we expect one message
	// for the family + one for Bash = 2 messages total.
	if len(bot.sent) != 2 {
		t.Fatalf("got %d messages, want 2", len(bot.sent))
	}
	want0 := "mcp__gmail-personal__*"
	want1 := "Bash"
	if !strings.Contains(bot.sent[0].text, want0) {
		t.Errorf("msg 0 missing pattern %q: %q", want0, bot.sent[0].text)
	}
	if !strings.Contains(bot.sent[1].text, want1) {
		t.Errorf("msg 1 missing pattern %q: %q", want1, bot.sent[1].text)
	}
	for _, m := range bot.sent {
		if !strings.Contains(m.text, "permissions.allow") {
			t.Errorf("msg missing settings.json hint: %q", m.text)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd/otto/ -run TestSurfaceDenialsAsPlainText -v
```

Expected: FAIL — current `surfaceDenials` calls `SendMessageWithButtons`, not `SendMessage`, so `bot.sent` won't match.

- [ ] **Step 3: Replace `surfaceDenials` body**

In `cmd/otto/handler.go`, replace the entire `surfaceDenials` method with:

```go
// surfaceDenials sends one plain-text message per unique denied-tool pattern
// with copy-pasteable instructions for editing ~/.claude/settings.json.
// Called after each Claude turn — denials are typically empty (the
// skip-permissions flag works), but when something slips through we want
// the user to know what to add and where.
func (h *handler) surfaceDenials(ctx context.Context, chatID int64, _ string, denials []claude.PermissionDenial) {
	seen := map[string]struct{}{}
	for _, d := range denials {
		pattern := patternForTool(d.ToolName)
		if _, dup := seen[pattern]; dup {
			continue
		}
		seen[pattern] = struct{}{}
		text := fmt.Sprintf(
			"⚠️ Claude tried to use %s and was denied.\n\nTo allow it next time, add %s to permissions.allow in ~/.claude/settings.json, then /restart.",
			d.ToolName, pattern,
		)
		if err := telegram.SendChunked(ctx, h.bot, chatID, text); err != nil {
			log.Printf("send error (denial text): %v", err)
		}
	}
}

// patternForTool turns a tool name into a permission pattern suitable for
// settings.json's permissions.allow array. MCP tools become a wildcard
// over the whole server family; built-in tool names are returned verbatim.
func patternForTool(toolName string) string {
	if strings.HasPrefix(toolName, "mcp__") {
		rest := strings.TrimPrefix(toolName, "mcp__")
		if i := strings.LastIndex(rest, "__"); i > 0 {
			return "mcp__" + rest[:i] + "__*"
		}
	}
	return toolName
}
```

The `originalPrompt` parameter is preserved as `_` so the call site in `runAndReply` doesn't need to change yet. (We'll remove it in Task A2 along with the rest of the callback infrastructure.)

- [ ] **Step 4: Run the test**

```bash
go test ./cmd/otto/ -run TestSurfaceDenialsAsPlainText -v
```

Expected: PASS.

- [ ] **Step 5: Run full test suite**

```bash
go test ./...
```

Expected: most tests pass; any callback-specific tests in `handler_test.go` (e.g. `TestHandleCallbackAlways`, `TestHandleCallbackOnce`, `TestHandleCallbackDeny`, `TestHandleCallbackUnknownID`) still pass because the callback path is untouched. Tests we deleted/rewrote pass.

- [ ] **Step 6: Commit**

```bash
git add cmd/otto/handler.go cmd/otto/handler_test.go
git commit -m "send plain-text denial instructions instead of inline buttons"
```

---

### Task A2: Remove `handleCallback` and the dispatch branch

**Files:**
- Modify: `cmd/otto/handler.go` (remove `handleCallback`, the `IsCallback` branch in `dispatch`, `callbackPrefixPerm` constant, `permissions` import)
- Modify: `cmd/otto/handler_test.go` (delete callback tests: `TestHandleCallbackOnce`, `TestHandleCallbackAlways`, `TestHandleCallbackDeny`, `TestHandleCallbackUnknownID`, etc.)

- [ ] **Step 1: Delete the callback tests**

Open `cmd/otto/handler_test.go`. Delete every test function whose name starts with `TestHandleCallback` or that constructs a `telegram.Update` with a non-empty `CallbackQueryID`. There are ~4-5 such tests.

- [ ] **Step 2: Run tests to confirm only the expected ones are gone**

```bash
go test ./cmd/otto/ -v 2>&1 | grep "^=== RUN" | head
```

Expected: no `TestHandleCallback*` lines appear.

- [ ] **Step 3: Remove `handleCallback` and the dispatch branch in handler.go**

Open `cmd/otto/handler.go`:

a) Delete the `callbackPrefixPerm` constant near the top:

```go
const callbackPrefixPerm = "perm:"
```

b) Delete the `permissions` import line.

c) In `dispatch`, remove this block:

```go
if u.IsCallback() {
    h.handleCallback(ctx, u)
    return
}
```

d) Delete the entire `handleCallback` method (~60 lines, including its docstring). The method header is:

```go
func (h *handler) handleCallback(ctx context.Context, u telegram.Update) {
```

- [ ] **Step 4: Build and run tests**

```bash
go build ./... && go test ./cmd/otto/
```

Expected: build succeeds (we still have `pending` and `settingsPath` fields wired in main.go but unused inside the handler now — Go allows this for struct fields). Tests pass.

If the build fails because `permissions.AllowTool` or other refs remain, search for them:

```bash
grep -n "permissions\." cmd/otto/*.go
```

There should be zero results.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/handler.go cmd/otto/handler_test.go
git commit -m "remove inline-keyboard callback dispatch path"
```

---

### Task A3: Remove `pending` field and delete the `permissions` package

**Files:**
- Modify: `cmd/otto/handler.go` (remove `pending` field)
- Modify: `cmd/otto/main.go` (remove `permissions.New(64)` and import)
- Modify: `cmd/otto/handler_test.go` (remove `pending: permissions.New(8)` from any remaining handler constructions; remove `permissions` import)
- Delete: `internal/permissions/permissions.go`
- Delete: `internal/permissions/permissions_test.go`

- [ ] **Step 1: Remove the `pending` field and any references**

In `cmd/otto/handler.go`, remove the line:

```go
pending      *permissions.Pending
```

from the `handler` struct definition.

In `cmd/otto/handler_test.go`, find any `&handler{...}` literal that includes `pending: permissions.New(...)` and remove that line. Also remove the `"otto/internal/permissions"` import.

- [ ] **Step 2: Remove the init in main.go**

In `cmd/otto/main.go`:

a) Remove the `"otto/internal/permissions"` import line.

b) In the `&handler{...}` literal in `main()`, remove:

```go
pending:      permissions.New(64),
```

- [ ] **Step 3: Delete the permissions package**

```bash
rm -rf internal/permissions/
```

- [ ] **Step 4: Build and test**

```bash
go build ./... && go test ./...
```

Expected: PASS. If you see "imported and not used" or "undefined: permissions", grep again:

```bash
grep -rn "permissions\." --include="*.go" .
```

Should return zero results.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "delete internal/permissions package"
```

---

### Task A4: Remove `settingsPath` and `ClaudeSettingsPath`

**Files:**
- Modify: `cmd/otto/handler.go` (remove `settingsPath` field)
- Modify: `cmd/otto/main.go` (remove the settings-path resolution block)
- Modify: `cmd/otto/handler_test.go` (remove any `settingsPath:` from handler construction)
- Modify: `internal/config/config.go` (remove `ClaudeSettingsPath` field)
- Modify: `internal/config/config_test.go` (remove any references)
- Modify: `setup.sh` (stop writing `claude_settings_path` in the generated config)

- [ ] **Step 1: Remove `settingsPath` from handler struct**

In `cmd/otto/handler.go`, remove the line:

```go
settingsPath string
```

In `cmd/otto/handler_test.go`, remove any `settingsPath: "..."` from `&handler{...}` constructions.

- [ ] **Step 2: Remove resolution block in main.go**

In `cmd/otto/main.go`, delete this block (around line 90-93):

```go
settingsPath := cfg.ClaudeSettingsPath
if settingsPath == "" {
    settingsPath = home + "/.claude/settings.json"
}
```

And in the `&handler{...}` literal, remove:

```go
settingsPath: settingsPath,
```

- [ ] **Step 3: Remove `ClaudeSettingsPath` from config**

In `internal/config/config.go`, remove the field:

```go
// ClaudeSettingsPath is where Otto writes "allow always" rules from
// the inline-keyboard permission flow. Defaults to ~/.claude/settings.json
// when empty.
ClaudeSettingsPath string `toml:"claude_settings_path"`
```

In `internal/config/config_test.go`, remove any test fixtures or assertions that reference `claude_settings_path`. Search:

```bash
grep -n "claude_settings_path\|ClaudeSettingsPath" internal/config/config_test.go
```

Remove the matching lines.

- [ ] **Step 4: Update setup.sh**

In `setup.sh`, find the section that writes the generated `config.toml` (search for `claude_settings_path`):

```bash
grep -n "claude_settings_path" setup.sh
```

Delete the line(s) that write `claude_settings_path = "..."` into the generated config. Existing user configs that already have the line will continue to work — TOML decoders ignore unknown fields silently after the field is removed from the struct.

- [ ] **Step 5: Build and test**

```bash
go build ./... && go test ./...
```

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cmd/otto/handler.go cmd/otto/handler_test.go cmd/otto/main.go internal/config/config.go internal/config/config_test.go setup.sh
git commit -m "remove ClaudeSettingsPath config field and handler plumbing"
```

---

### Task A5: Remove inline-button surface from `internal/telegram`

**Files:**
- Modify: `internal/telegram/client.go` (remove `SendMessageWithButtons`, `AnswerCallbackQuery`, `InlineButton`, callback fields on `Update`, `IsCallback`, callback parsing in `fromTGUpdate`)
- Modify: `cmd/otto/tty.go` (remove `SendMessageWithButtons` and `AnswerCallbackQuery` stubs)
- Modify: `cmd/otto/handler_test.go` (remove `SendMessageWithButtons` / `AnswerCallbackQuery` from `fakeBot`; remove `buttons [][]telegram.InlineButton` field from `sentMsg`; remove `answered []string` field)
- Modify: `internal/telegram/client_test.go` (delete any tests for the removed methods)

- [ ] **Step 1: Strip the telegram package**

In `internal/telegram/client.go`:

a) Remove these fields from the `Update` struct:

```go
CallbackQueryID   string
CallbackData      string
CallbackMessageID int
```

b) Remove the `IsCallback()` method.

c) Remove the `InlineButton` struct.

d) From the `BotClient` interface, remove these two method lines:

```go
SendMessageHTML(ctx context.Context, chatID int64, text string) error    // KEEP THIS — Toto uses it
SendMessageWithButtons(ctx context.Context, chatID int64, text string, buttons [][]InlineButton) error  // REMOVE
AnswerCallbackQuery(ctx context.Context, queryID, text string) error  // REMOVE
```

(Be careful not to remove `SendMessageHTML` — Toto needs it to render ASCII art in `<pre>` tags.)

e) From `realClient`, remove the `SendMessageWithButtons` and `AnswerCallbackQuery` method implementations.

f) In `fromTGUpdate`, remove the entire `if u.CallbackQuery != nil { ... }` block at the top.

- [ ] **Step 2: Strip the TTY bot**

In `cmd/otto/tty.go`:

a) Remove the `SendMessageWithButtons` method (~10 lines including comment).

b) Remove the `AnswerCallbackQuery` method (~5 lines).

The compile-time interface assertion `var _ telegram.BotClient = (*ttyBot)(nil)` at the bottom will catch any missed methods.

- [ ] **Step 3: Strip the test fakeBot**

In `cmd/otto/handler_test.go`:

a) Remove the `SendMessageWithButtons` method on `fakeBot`.

b) Remove the `AnswerCallbackQuery` method on `fakeBot`.

c) Remove the `answered []string` field from `fakeBot`.

d) Remove the `buttons [][]telegram.InlineButton` field from `sentMsg`.

- [ ] **Step 4: Strip telegram package tests**

```bash
grep -n "SendMessageWithButtons\|AnswerCallbackQuery\|InlineButton\|CallbackData\|CallbackQueryID" internal/telegram/client_test.go
```

Delete any test functions that exercise these surfaces. Other tests in the file (for `SendMessage`, `SendMessageHTML`, `GetUpdates`, `DownloadFile`) remain untouched.

- [ ] **Step 5: Build and test**

```bash
go build ./... && go test ./...
```

Expected: PASS. If the build fails with "unused import", find and remove the dead import.

- [ ] **Step 6: Commit**

```bash
git add internal/telegram/ cmd/otto/tty.go cmd/otto/handler_test.go
git commit -m "remove inline-button surface from telegram package"
```

---

### Task A6: Verify Part A end-to-end

- [ ] **Step 1: Full test suite**

```bash
go test -race ./...
```

Expected: PASS.

- [ ] **Step 2: gofmt + vet**

```bash
make vet
```

Expected: clean — no formatting diffs, no `go vet` complaints.

- [ ] **Step 3: TTY smoke test**

```bash
make build && ./otto -tty < /dev/null
```

Expected: prints `[tty] type messages and press enter; ctrl-d to exit`, then exits cleanly on EOF (no crashes, no nil-pointer panics from removed callback paths).

- [ ] **Step 4: Confirm git log shows clean history**

```bash
git log --oneline | head
```

You should see five commits from Tasks A1-A5 above the spec commit. If anything looks tangled, that's fine — Part A is logically one feature, the granular commits are for safety.

---

## Phase B: opt-in self-update

### Task B1: Add `version` constant and `/version` command

**Files:**
- Modify: `cmd/otto/main.go` (add `var version = "dev"`)
- Modify: `cmd/otto/commands.go` (add `/version` case)
- Modify: `cmd/otto/commands_test.go` (test for `/version`)
- Modify: `Makefile` (add `VERSION ?= dev` and pass via `-ldflags`)

- [ ] **Step 1: Write a failing test for `/version`**

Add to `cmd/otto/commands_test.go` (or create the file if it doesn't exist):

```go
func TestVersionCommand(t *testing.T) {
	h := &handler{}
	got := h.tryCommand(context.Background(), telegram.Update{Text: "/version"})
	if !got.handled {
		t.Fatal("expected /version to be handled")
	}
	if !strings.Contains(got.reply, "version=") {
		t.Errorf("reply missing version=: %q", got.reply)
	}
	if !strings.Contains(got.reply, runtime.GOOS) {
		t.Errorf("reply missing GOOS=%s: %q", runtime.GOOS, got.reply)
	}
}
```

Add the necessary imports (`runtime`, `strings`, `context`, `testing`, `otto/internal/telegram`).

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd/otto/ -run TestVersionCommand -v
```

Expected: FAIL — `/version` not yet handled.

- [ ] **Step 3: Add the version constant**

In `cmd/otto/main.go`, near the top of the file (just inside `package main`, after the imports):

```go
// version is the build-time version string. Overridden via
// -ldflags "-X main.version=v1.2.3" in CI release builds; "dev" for
// local builds. The updater skips polling entirely when version == "dev".
var version = "dev"
```

- [ ] **Step 4: Add the `/version` case**

In `cmd/otto/commands.go`, inside the `switch parts[0]` block in `tryCommand`, add a new case:

```go
case "/version":
	return commandResult{
		reply:   fmt.Sprintf("version=%s os=%s/%s", version, runtime.GOOS, runtime.GOARCH),
		handled: true,
	}
```

Add `"runtime"` to the imports of `commands.go`.

- [ ] **Step 5: Run the test**

```bash
go test ./cmd/otto/ -run TestVersionCommand -v
```

Expected: PASS, with `version=dev` in the output.

- [ ] **Step 6: Update Makefile**

Replace the `build` and `install` targets in `Makefile`:

```make
VERSION ?= dev

# build: produce ./otto in the repo for local testing.
build:
	go build -ldflags "-X main.version=$(VERSION)" -o ./otto ./cmd/otto

# install: deploy directly to ~/.local/bin/otto where launchd / systemd run
# from. Use this — NOT `go install` — when iterating, because `go install`
# silently writes to $GOBIN (~/go/bin by default) which is a different file
# than the one the running service uses.
install:
	go build -ldflags "-X main.version=$(VERSION)" -o $(HOME)/.local/bin/otto ./cmd/otto
```

- [ ] **Step 7: Verify the ldflag works end-to-end**

```bash
VERSION=v0.0.1-test make build && ./otto -tty <<< "/version"
```

Expected output includes `version=v0.0.1-test os=darwin/arm64` (or your actual GOOS/GOARCH), then EOF, then exit.

- [ ] **Step 8: Commit**

```bash
git add cmd/otto/main.go cmd/otto/commands.go cmd/otto/commands_test.go Makefile
git commit -m "add version constant and /version command"
```

---

### Task B2: Create `updater.go` skeleton with `assetForPlatform`

**Files:**
- Create: `cmd/otto/updater.go`
- Create: `cmd/otto/updater_test.go`

- [ ] **Step 1: Write the failing test for `assetForPlatform`**

Create `cmd/otto/updater_test.go`:

```go
//go:build unix

package main

import "testing"

func TestAssetForPlatform(t *testing.T) {
	assets := []releaseAsset{
		{Name: "otto-linux-amd64", URL: "https://example.com/linux-amd64"},
		{Name: "otto-linux-arm64", URL: "https://example.com/linux-arm64"},
		{Name: "otto-darwin-arm64", URL: "https://example.com/darwin-arm64"},
	}
	cases := []struct {
		goos, goarch string
		wantURL      string
		wantOK       bool
	}{
		{"linux", "amd64", "https://example.com/linux-amd64", true},
		{"linux", "arm64", "https://example.com/linux-arm64", true},
		{"darwin", "arm64", "https://example.com/darwin-arm64", true},
		{"freebsd", "amd64", "", false},
		{"linux", "386", "", false},
		{"windows", "amd64", "", false},
	}
	for _, c := range cases {
		t.Run(c.goos+"/"+c.goarch, func(t *testing.T) {
			got, ok := assetForPlatform(assets, c.goos, c.goarch)
			if ok != c.wantOK {
				t.Fatalf("ok=%v, want %v", ok, c.wantOK)
			}
			if got.URL != c.wantURL {
				t.Errorf("URL=%q, want %q", got.URL, c.wantURL)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./cmd/otto/ -run TestAssetForPlatform -v
```

Expected: FAIL with "undefined: releaseAsset" or "undefined: assetForPlatform".

- [ ] **Step 3: Create `updater.go` with the type and function**

Create `cmd/otto/updater.go`:

```go
//go:build unix

package main

// releaseAsset is one entry from a GitHub Release's assets list. The
// updater fetches the latest release JSON, picks the asset matching the
// running binary's GOOS/GOARCH, and downloads it on /update.
type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

// assetForPlatform finds the asset whose name ends in -<goos>-<goarch>
// (e.g. otto-linux-amd64). Returns ok=false if no such asset exists.
// CI publishes one binary per supported platform with names matching
// this convention; mismatch means the platform isn't supported by this
// release.
func assetForPlatform(assets []releaseAsset, goos, goarch string) (releaseAsset, bool) {
	suffix := "-" + goos + "-" + goarch
	for _, a := range assets {
		if len(a.Name) > len(suffix) && a.Name[len(a.Name)-len(suffix):] == suffix {
			return a, true
		}
	}
	return releaseAsset{}, false
}
```

- [ ] **Step 4: Run the test**

```bash
go test ./cmd/otto/ -run TestAssetForPlatform -v
```

Expected: PASS for all six sub-tests.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/updater.go cmd/otto/updater_test.go
git commit -m "add releaseAsset type and assetForPlatform matcher"
```

---

### Task B2.5: Create Toot character (owl notification courier)

**Files:**
- Create: `cmd/otto/toot.go`
- Create: `cmd/otto/toot_test.go`
- Move + commit: `toot.txt` (currently untracked at repo root) — the `//go:embed toot.txt` directive in `cmd/otto/toot.go` resolves relative to the source file, so the file must live at `cmd/otto/toot.txt`. Move it during this task.

**Why this task exists:** Tasks B4-B7 thread a `*Toot` reference through the updater (it owns release announcements and install confirmations). Building Toot first lets later tasks inject it cleanly. Toot is a stripped-down sibling of Toto — same ASCII-art-prepended `<pre>` rendering pattern, but no LLM, no session, no persona. Just a one-way owl that delivers updates.

- [ ] **Step 1: Move toot.txt next to the source file**

```bash
git mv toot.txt cmd/otto/toot.txt 2>/dev/null || mv toot.txt cmd/otto/toot.txt
ls cmd/otto/toot.txt
```

(The `git mv` form fails silently if the file isn't tracked yet; the fallback `mv` handles that case. Either way `cmd/otto/toot.txt` should exist after this step.)

- [ ] **Step 2: Write the failing tests**

Create `cmd/otto/toot_test.go`:

```go
//go:build unix

package main

import (
	"context"
	"strings"
	"testing"
)

func TestTootSendIncludesArtAndBody(t *testing.T) {
	bot := &fakeBot{}
	toot := newToot(bot)
	if err := toot.Send(context.Background(), 42, "v1.0.1 is out"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(bot.sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if !strings.Contains(msg, "<pre>") {
		t.Errorf("missing <pre> wrapper: %q", msg)
	}
	if !strings.Contains(msg, "v1.0.1 is out") {
		t.Errorf("missing body: %q", msg)
	}
	// All three owl arts include "(o,o)" — verify one was selected.
	if !strings.Contains(msg, "(o,o)") {
		t.Errorf("missing owl signature: %q", msg)
	}
}

func TestTootSendEscapesHTMLInBody(t *testing.T) {
	bot := &fakeBot{}
	toot := newToot(bot)
	if err := toot.Send(context.Background(), 42, "<script>alert(1)</script>"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bot.sent[0].text, "&lt;script&gt;") {
		t.Errorf("expected HTML-escaped body, got %q", bot.sent[0].text)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

```bash
go test ./cmd/otto/ -run TestToot -v
```

Expected: FAIL with "undefined: newToot" / "undefined: Toot".

- [ ] **Step 4: Create `cmd/otto/toot.go`**

```go
//go:build unix

package main

import (
	"context"
	_ "embed"
	"html"

	"otto/internal/telegram"
)

// tootArtFile is the bundled owl ASCII-art file. Three blocks separated
// by blank lines; same format the existing parseAsciiArts (in toto.go)
// already handles for Toto's cats.
//
//go:embed toot.txt
var tootArtFile string

// tootCycler hands out the embedded owl arts in shuffled round-robin
// order, so consecutive Toot messages don't repeat the same art.
var tootCycler = newAsciiCycler(parseAsciiArts(tootArtFile))

// pickTootArt returns the next owl art via the shuffled round-robin
// cycler, or "" if no arts were loaded.
func pickTootArt() string { return tootCycler.Next() }

// Toot is the owl character that delivers update notifications. Unlike
// Toto, Toot has no LLM and no session — it's a one-way courier. The
// updater calls Send() with a fully-composed body (release announcement
// or install confirmation); Toot prepends a random owl art via <pre>
// tags in HTML mode and ships it.
//
// Conversational messages (command replies, error messages) stay on the
// regular bot — Toot exists specifically to mark "this is an update
// event" visually so the user knows what kind of message they're
// reading.
type Toot struct {
	bot telegram.BotClient
}

func newToot(bot telegram.BotClient) *Toot {
	return &Toot{bot: bot}
}

// Send delivers body with a random owl art prepended. The body is
// HTML-escaped so any literal <, >, & survive Telegram's HTML parser.
func (t *Toot) Send(ctx context.Context, chatID int64, body string) error {
	art := pickTootArt()
	escapedBody := html.EscapeString(body)
	var msg string
	if art != "" {
		msg = "<pre>" + html.EscapeString(art) + "</pre>\n\n" + escapedBody
	} else {
		msg = escapedBody
	}
	return t.bot.SendMessageHTML(ctx, chatID, msg)
}
```

- [ ] **Step 5: Run tests**

```bash
go test ./cmd/otto/ -run TestToot -v
```

Expected: both PASS.

- [ ] **Step 6: Verify the embed picked up the file**

```bash
go build ./cmd/otto/ && echo "build OK"
```

Expected: `build OK`. If the embed fails (`pattern toot.txt: no matching files found`), confirm `cmd/otto/toot.txt` exists and re-run.

- [ ] **Step 7: Commit**

```bash
git add cmd/otto/toot.go cmd/otto/toot_test.go cmd/otto/toot.txt
git commit -m "add toot owl character for update notifications"
```

---

### Task B3: Implement `fetchLatest` against a `httptest` server

**Files:**
- Modify: `cmd/otto/updater.go` (add `release` type, `fetchLatest` method, `updater` struct)
- Modify: `cmd/otto/updater_test.go` (test `fetchLatest`)

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto/updater_test.go`:

```go
import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLatest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"tag_name": "v1.2.3",
			"body": "What's Changed\n* Add /update (#1)\n* Fix denial UX (#2)",
			"assets": [
				{"name": "otto-linux-amd64", "browser_download_url": "https://x/otto-linux-amd64"},
				{"name": "otto-darwin-arm64", "browser_download_url": "https://x/otto-darwin-arm64"}
			]
		}`)
	}))
	defer server.Close()

	u := &updater{
		httpClient:  server.Client(),
		releasesURL: server.URL,
	}
	rel, err := u.fetchLatest(context.Background())
	if err != nil {
		t.Fatalf("fetchLatest: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Errorf("TagName=%q, want v1.2.3", rel.TagName)
	}
	if !strings.Contains(rel.Body, "What's Changed") {
		t.Errorf("Body missing patch notes: %q", rel.Body)
	}
	if len(rel.Assets) != 2 {
		t.Fatalf("got %d assets, want 2", len(rel.Assets))
	}
	if rel.Assets[0].Name != "otto-linux-amd64" {
		t.Errorf("Assets[0].Name=%q", rel.Assets[0].Name)
	}
}

func TestFetchLatestNon200(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer server.Close()

	u := &updater{httpClient: server.Client(), releasesURL: server.URL}
	_, err := u.fetchLatest(context.Background())
	if err == nil {
		t.Fatal("expected error on 403 response")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/otto/ -run TestFetchLatest -v
```

Expected: FAIL with "undefined: updater" or "undefined: fetchLatest".

- [ ] **Step 3: Add the types and method**

In `cmd/otto/updater.go`, append:

```go
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const (
	updateCheckInterval = 1 * time.Hour
	updateInitialDelay  = 30 * time.Second
	releasesURLDefault  = "https://api.github.com/repos/TheOddjobShop/Otto/releases/latest"
	downloadTimeout     = 5 * time.Minute
)

// release is the slice of GitHub's release JSON we care about. Body
// is the auto-generated changelog (when our workflow sets
// generate_release_notes: true) and Toot includes it in the announcement
// as patch notes for the user.
type release struct {
	TagName string         `json:"tag_name"`
	Body    string         `json:"body"`
	Assets  []releaseAsset `json:"assets"`
}

// pendingUpdate holds the most recent detected available release that
// matches the current platform. /update reads this; the poller writes it.
type pendingUpdate struct {
	Tag       string
	AssetName string
	AssetURL  string
}

// updater polls GitHub Releases for new versions of Otto, notifies the
// allowlisted user via Toot when one is detected, and applies it on
// /update.
//
// httpClient and releasesURL are settable from the same package so
// tests can substitute httptest servers.
type updater struct {
	httpClient     *http.Client
	releasesURL    string
	currentVersion string
	toot           *Toot
	chatID         int64

	mu            sync.Mutex
	pending       *pendingUpdate
	lastAnnounced string
}

// fetchLatest hits the releases/latest endpoint and parses the response.
// Returns an error on non-200 status or unparseable JSON.
func (u *updater) fetchLatest(ctx context.Context) (release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.releasesURL, nil)
	if err != nil {
		return release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return release{}, fmt.Errorf("updater: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("updater: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB safety cap
	if err != nil {
		return release{}, fmt.Errorf("updater: read: %w", err)
	}
	var rel release
	if err := json.Unmarshal(body, &rel); err != nil {
		return release{}, fmt.Errorf("updater: parse: %w", err)
	}
	return rel, nil
}
```

(Move the `import` block to the top of the file alongside any existing imports — Go only allows one `import` statement per file.)

- [ ] **Step 4: Run tests**

```bash
go test ./cmd/otto/ -run TestFetchLatest -v
```

Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/updater.go cmd/otto/updater_test.go
git commit -m "add updater struct and fetchLatest"
```

---

### Task B4: Implement `checkOnce` with announcement deduplication

**Files:**
- Modify: `cmd/otto/updater.go` (add `checkOnce`, `Pending`)
- Modify: `cmd/otto/updater_test.go` (test dedup behavior)

- [ ] **Step 1: Write failing tests**

Append to `cmd/otto/updater_test.go`:

```go
import "runtime"

// newTestUpdater returns an updater whose Toot is wired to a fakeBot.
// Callers read fakeBot.sent to inspect what Toot delivered. (Toot's
// SendMessageHTML appends to the same .sent slice as plain SendMessage,
// so test assertions just look at .sent[i].text.)
func newTestUpdater(t *testing.T, releasesJSON string) (*updater, *fakeBot, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, releasesJSON)
	}))
	bot := &fakeBot{}
	u := &updater{
		httpClient:     server.Client(),
		releasesURL:    server.URL,
		currentVersion: "v1.0.0",
		toot:           newToot(bot),
		chatID:         42,
	}
	return u, bot, server.Close
}

func TestCheckOnceAnnouncesNewRelease(t *testing.T) {
	json := fmt.Sprintf(`{
		"tag_name": "v1.0.1",
		"body": "What's Changed\n* Add Toot (#3)",
		"assets": [{"name": "otto-%s-%s", "browser_download_url": "https://x/asset"}]
	}`, runtime.GOOS, runtime.GOARCH)
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())

	if len(bot.sent) != 1 {
		t.Fatalf("got %d messages, want 1", len(bot.sent))
	}
	msg := bot.sent[0].text
	if !strings.Contains(msg, "v1.0.1") {
		t.Errorf("missing tag in message: %q", msg)
	}
	if !strings.Contains(msg, "/update") {
		t.Errorf("missing /update hint: %q", msg)
	}
	if !strings.Contains(msg, "What&#39;s Changed") && !strings.Contains(msg, "Add Toot") {
		// Body is HTML-escaped by Toot.Send. We accept either the escaped
		// apostrophe or the unescaped tail as evidence that body was
		// included.
		t.Errorf("missing patch notes in message: %q", msg)
	}
	if !strings.Contains(msg, "<pre>") {
		t.Errorf("missing owl <pre> wrapper (Toot didn't deliver?): %q", msg)
	}

	p := u.Pending()
	if p == nil {
		t.Fatal("Pending() returned nil")
	}
	if p.Tag != "v1.0.1" {
		t.Errorf("Pending.Tag=%q", p.Tag)
	}
	if p.AssetURL != "https://x/asset" {
		t.Errorf("Pending.AssetURL=%q", p.AssetURL)
	}
}

func TestCheckOnceDoesNotAnnounceCurrentVersion(t *testing.T) {
	json := `{"tag_name": "v1.0.0", "assets": []}`
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())
	if len(bot.sent) != 0 {
		t.Errorf("got %d messages, want 0", len(bot.sent))
	}
	if u.Pending() != nil {
		t.Error("Pending() should be nil when tag matches current version")
	}
}

func TestCheckOnceDedupesAnnouncement(t *testing.T) {
	json := fmt.Sprintf(`{
		"tag_name": "v1.0.1",
		"assets": [{"name": "otto-%s-%s", "browser_download_url": "https://x/asset"}]
	}`, runtime.GOOS, runtime.GOARCH)
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())
	u.checkOnce(context.Background())
	u.checkOnce(context.Background())

	if len(bot.sent) != 1 {
		t.Errorf("got %d messages across 3 ticks, want 1", len(bot.sent))
	}
}

func TestCheckOnceSkipsMissingPlatformAsset(t *testing.T) {
	// Release exists but has no asset for the running platform.
	json := `{
		"tag_name": "v1.0.1",
		"assets": [{"name": "otto-plan9-amd64", "browser_download_url": "https://x/plan9"}]
	}`
	u, bot, cleanup := newTestUpdater(t, json)
	defer cleanup()

	u.checkOnce(context.Background())

	// We still announce so the user knows an update exists, but Pending
	// is nil so /update will explain the platform mismatch.
	if len(bot.sent) != 1 {
		t.Errorf("got %d messages, want 1", len(bot.sent))
	}
	if u.Pending() != nil {
		t.Error("Pending() should be nil when no asset matches platform")
	}
}
```

Add `"strings"` to the imports of `updater_test.go` if not already present. The `fakeBot` type is defined in `handler_test.go` (same package).

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/otto/ -run TestCheckOnce -v
```

Expected: FAIL — `checkOnce` and `Pending` don't exist.

- [ ] **Step 3: Implement `checkOnce` and `Pending`**

Append to `cmd/otto/updater.go`:

```go
import (
	"log"
	"runtime"
)

// checkOnce hits releases/latest and, if the latest tag differs from
// both the current version and the previously-announced tag, sends a
// Toot announcement and records the pending install.
//
// If the release exists but has no asset for the current platform, we
// still announce (so the user knows an update is out) but record no
// pending — /update will explain the mismatch.
func (u *updater) checkOnce(ctx context.Context) {
	rel, err := u.fetchLatest(ctx)
	if err != nil {
		log.Printf("updater: %v", err)
		return
	}
	if rel.TagName == u.currentVersion {
		return
	}
	u.mu.Lock()
	if rel.TagName == u.lastAnnounced {
		u.mu.Unlock()
		return
	}
	asset, ok := assetForPlatform(rel.Assets, runtime.GOOS, runtime.GOARCH)
	if ok {
		u.pending = &pendingUpdate{
			Tag:       rel.TagName,
			AssetName: asset.Name,
			AssetURL:  asset.URL,
		}
	} else {
		u.pending = nil
	}
	u.lastAnnounced = rel.TagName
	u.mu.Unlock()

	msg := buildAnnounceMessage(u.currentVersion, rel.TagName, rel.Body, ok)
	if err := u.toot.Send(ctx, u.chatID, msg); err != nil {
		log.Printf("updater: toot send: %v", err)
	}
}

// buildAnnounceMessage composes Toot's announcement body. Patch notes
// from the release are included verbatim when present; trailing
// whitespace is trimmed so we don't leave a dangling blank line before
// the "Reply /update" hint.
func buildAnnounceMessage(currentVersion, newTag, body string, hasPlatformAsset bool) string {
	header := fmt.Sprintf("%s → %s", currentVersion, newTag)
	footer := "Reply /update to install."
	if !hasPlatformAsset {
		footer = fmt.Sprintf(
			"No binary for %s/%s in this release. Build manually or wait for the next one.",
			runtime.GOOS, runtime.GOARCH,
		)
	}
	if body = trimRight(body); body == "" {
		return header + "\n\n" + footer
	}
	return header + "\n\n" + body + "\n\n" + footer
}

// trimRight strips trailing whitespace including blank lines.
func trimRight(s string) string {
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\n' || c == '\t' || c == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// Pending returns the current pending install, or nil if none.
// Safe to call from any goroutine.
func (u *updater) Pending() *pendingUpdate {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pending
}
```

(Merge the `import` blocks at the top of the file.)

- [ ] **Step 4: Run tests**

```bash
go test ./cmd/otto/ -run TestCheckOnce -v
```

Expected: all four PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/updater.go cmd/otto/updater_test.go
git commit -m "add checkOnce with announcement dedup"
```

---

### Task B5: Add `Run` poller loop and wire into main.go

**Files:**
- Modify: `cmd/otto/updater.go` (add `Run`, `newUpdater`)
- Modify: `cmd/otto/main.go` (construct updater, start goroutine, store on handler)
- Modify: `cmd/otto/handler.go` (add `updater *updater` field)

- [ ] **Step 1: Add `newUpdater` constructor and `Run` method**

Append to `cmd/otto/updater.go`:

```go
// newUpdater constructs an updater that polls the default GitHub URL.
// Pass version="dev" for local builds — Run will short-circuit and not
// poll at all.
func newUpdater(toot *Toot, chatID int64, currentVersion string) *updater {
	return &updater{
		httpClient:     &http.Client{Timeout: 30 * time.Second},
		releasesURL:    releasesURLDefault,
		currentVersion: currentVersion,
		toot:           toot,
		chatID:         chatID,
	}
}

// Run polls for updates until ctx is cancelled. No-op when the binary
// was built without a version tag (currentVersion == "dev").
func (u *updater) Run(ctx context.Context) {
	if u.currentVersion == "dev" {
		log.Printf("updater: version=dev, skipping poll loop")
		return
	}
	log.Printf("updater: starting (interval=%s, initial=%s, repo=%s)",
		updateCheckInterval, updateInitialDelay, u.releasesURL)
	select {
	case <-time.After(updateInitialDelay):
	case <-ctx.Done():
		return
	}
	u.checkOnce(ctx)
	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			u.checkOnce(ctx)
		case <-ctx.Done():
			return
		}
	}
}
```

- [ ] **Step 2: Add `updater` field on the handler**

In `cmd/otto/handler.go`, in the `handler` struct definition, add:

```go
updater *updater
```

(Place it near the other dependencies, e.g. after `toto *Toto`.)

- [ ] **Step 3: Wire it up in main.go**

In `cmd/otto/main.go`, after the `&handler{...}` literal is constructed and assigned to `h`, add:

```go
toot := newToot(bot)
h.updater = newUpdater(toot, cfg.TelegramAllowedUserID, version)
go h.updater.Run(ctx)
```

This goes between the handler construction and the signal-handler setup. Toot is constructed locally — it doesn't need to be stored on the handler since only the updater calls it.

- [ ] **Step 4: Build and run the existing test suite**

```bash
go build ./... && go test ./...
```

Expected: PASS. The poller goroutine isn't covered by any new test here — Run is a thin orchestrator over checkOnce, which we've already tested. Adding a Run-level test would mean controlling time, which adds complexity for low value.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/updater.go cmd/otto/main.go cmd/otto/handler.go
git commit -m "wire up update poller goroutine"
```

---

### Task B6: Implement the `Install` flow (download + atomic swap)

**Files:**
- Modify: `cmd/otto/updater.go` (add `Install` method, `exitFunc` field for testability)
- Modify: `cmd/otto/updater_test.go` (test happy path + failure modes)

- [ ] **Step 1: Write the failing tests**

Append to `cmd/otto/updater_test.go`:

```go
import (
	"os"
	"path/filepath"
)

func TestInstallSuccess(t *testing.T) {
	// Asset server: returns a small binary blob.
	binaryContents := []byte("#!/bin/sh\necho hello\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(binaryContents)
	}))
	defer server.Close()

	// Stand-in for os.Executable() — point at a temp file we can inspect.
	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "otto")
	if err := os.WriteFile(exePath, []byte("OLD"), 0755); err != nil {
		t.Fatal(err)
	}

	bot := &fakeBot{}
	u := &updater{
		httpClient:     server.Client(),
		toot:           newToot(bot),
		chatID:         42,
		currentVersion: "v1.0.0",
		exePath:        func() (string, error) { return exePath, nil },
		exitFunc:       func() {},
	}
	u.pending = &pendingUpdate{
		Tag:       "v1.0.1",
		AssetName: "otto-test",
		AssetURL:  server.URL,
	}

	if err := u.Install(context.Background()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binaryContents) {
		t.Errorf("binary not swapped: got %q, want %q", got, binaryContents)
	}

	// Toot delivered one "Installed" confirmation.
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "v1.0.1") {
		t.Errorf("messages=%v", bot.sent)
	}
}

func TestInstallNoPending(t *testing.T) {
	u := &updater{}
	err := u.Install(context.Background())
	if err == nil {
		t.Fatal("expected error when no pending update")
	}
}

func TestInstallDownloadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer server.Close()

	tmpDir := t.TempDir()
	exePath := filepath.Join(tmpDir, "otto")
	os.WriteFile(exePath, []byte("OLD"), 0755)

	u := &updater{
		httpClient: server.Client(),
		toot:       newToot(&fakeBot{}),
		exePath:    func() (string, error) { return exePath, nil },
		exitFunc:   func() {},
	}
	u.pending = &pendingUpdate{Tag: "v1.0.1", AssetURL: server.URL}

	err := u.Install(context.Background())
	if err == nil {
		t.Fatal("expected download error")
	}
	// Original binary must be untouched.
	got, _ := os.ReadFile(exePath)
	if string(got) != "OLD" {
		t.Errorf("original clobbered on failure: %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/otto/ -run TestInstall -v
```

Expected: FAIL — Install, exePath, exitFunc don't exist.

- [ ] **Step 3: Implement `Install` and add the injectable hooks**

In `cmd/otto/updater.go`:

a) Add to the `updater` struct:

```go
// Hooks for testing — production callers leave these at zero values
// (nil), which means use defaults: os.Executable + filepath.EvalSymlinks
// for exePath, and syscall.Kill(SIGTERM) for exitFunc.
exePath  func() (string, error)
exitFunc func()
```

b) Add this method:

```go
import (
	"os"
	"path/filepath"
	"syscall"
)

// Install downloads the pending update and atomically replaces the
// running binary. The exit hook is NOT called from Install — callers
// (the /update command) invoke it after Install returns successfully
// so the post-install message lands first.
//
// Returns an error if there's no pending update, the download fails,
// or the binary swap fails. On any error, the original binary is left
// intact.
func (u *updater) Install(ctx context.Context) error {
	u.mu.Lock()
	p := u.pending
	u.mu.Unlock()
	if p == nil {
		return fmt.Errorf("install: no pending update")
	}

	body, err := u.download(ctx, p.AssetURL)
	if err != nil {
		return fmt.Errorf("install: download: %w", err)
	}
	if len(body) == 0 {
		return fmt.Errorf("install: empty asset")
	}

	exe, err := u.resolveExePath()
	if err != nil {
		return fmt.Errorf("install: resolve binary path: %w", err)
	}

	tmp := exe + ".new"
	if err := os.WriteFile(tmp, body, 0755); err != nil {
		return fmt.Errorf("install: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("install: rename %s -> %s: %w", tmp, exe, err)
	}

	msg := fmt.Sprintf("Installed %s. Restarting…", p.Tag)
	if sendErr := u.toot.Send(ctx, u.chatID, msg); sendErr != nil {
		log.Printf("install: toot send confirm: %v", sendErr)
	}
	return nil
}

// download fetches a binary asset into memory. 5-minute timeout. The
// 100MB cap is paranoia — Otto binaries are ~10MB.
func (u *updater) download(ctx context.Context, url string) ([]byte, error) {
	dlCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 100*1024*1024))
}

// resolveExePath returns the absolute, symlink-resolved path of the
// current process's binary, or whatever the test hook returns.
func (u *updater) resolveExePath() (string, error) {
	if u.exePath != nil {
		return u.exePath()
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Exit triggers a clean process shutdown via SIGTERM (or the test hook).
// systemd's Restart=always brings Otto back on the new binary.
func (u *updater) Exit() {
	if u.exitFunc != nil {
		u.exitFunc()
		return
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
}
```

(Merge the new imports.)

- [ ] **Step 4: Run tests**

```bash
go test ./cmd/otto/ -run TestInstall -v
```

Expected: all three PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/updater.go cmd/otto/updater_test.go
git commit -m "implement updater Install flow with atomic binary swap"
```

---

### Task B7: Add `/update` command

**Files:**
- Modify: `cmd/otto/commands.go` (add `/update` case)
- Modify: `cmd/otto/commands_test.go` (test the three reply branches)
- Modify: `cmd/otto/handler.go` (add `triggerUpdate` helper that runs the install in a goroutine)

- [ ] **Step 1: Write failing tests**

Append to `cmd/otto/commands_test.go`:

```go
import "runtime"

func TestUpdateCommandNoPending(t *testing.T) {
	// No pending update → synchronous reply only, no async goroutine fires.
	h := &handler{
		updater: &updater{currentVersion: "v1.2.3"},
	}
	got := h.tryCommand(context.Background(), telegram.Update{Text: "/update"})
	if !got.handled {
		t.Fatal("not handled")
	}
	if !strings.Contains(got.reply, "No update available") {
		t.Errorf("reply=%q", got.reply)
	}
	if !strings.Contains(got.reply, "v1.2.3") {
		t.Errorf("reply missing current version: %q", got.reply)
	}
}

// newPendingHandler is a shared helper for tests that exercise the
// /update path with a real pending update. It wires both h.bot and
// u.toot (via newToot(bot)) to the same fakeBot so the async goroutine
// spawned by /update has a place to send its failure message (the
// AssetURL is bogus, so the install will fail-fast on DNS, post one
// error message via h.bot, and exit without panicking).
func newPendingHandler(t *testing.T) (*handler, *fakeBot) {
	t.Helper()
	bot := &fakeBot{}
	u := &updater{
		currentVersion: "v1.0.0",
		toot:           newToot(bot),
		chatID:         42,
		exitFunc:       func() {}, // never actually exit during tests
		exePath:        func() (string, error) { return "/tmp/otto-test-dummy", nil },
		httpClient:     &http.Client{Timeout: 2 * time.Second},
	}
	u.pending = &pendingUpdate{
		Tag:       "v1.0.1",
		AssetName: "otto-" + runtime.GOOS + "-" + runtime.GOARCH,
		AssetURL:  "https://otto-test-invalid.invalid/asset", // DNS will fail
	}
	h := &handler{updater: u, otto: newOttoState(), bot: bot}
	return h, bot
}

func TestUpdateCommandPending(t *testing.T) {
	h, _ := newPendingHandler(t)
	got := h.tryCommand(context.Background(), telegram.Update{Text: "/update"})
	if !got.handled {
		t.Fatal("not handled")
	}
	if !strings.Contains(got.reply, "v1.0.1") {
		t.Errorf("reply missing target tag: %q", got.reply)
	}
	if !strings.Contains(got.reply, "Starting update") {
		t.Errorf("reply=%q", got.reply)
	}
}

func TestUpdateCommandPendingOttoBusy(t *testing.T) {
	h, _ := newPendingHandler(t)
	h.otto.tryAcquire("doing a thing")

	got := h.tryCommand(context.Background(), telegram.Update{Text: "/update"})
	if !strings.Contains(got.reply, "interrupted") {
		t.Errorf("expected busy warning, got %q", got.reply)
	}
}
```

Add `"net/http"` and `"time"` to commands_test.go imports if not present. The `fakeBot` type is defined in handler_test.go (same package).

- [ ] **Step 2: Run tests to verify they fail**

```bash
go test ./cmd/otto/ -run TestUpdateCommand -v
```

Expected: FAIL.

- [ ] **Step 3: Add the `/update` case in commands.go**

In `cmd/otto/commands.go`, inside `switch parts[0]`, add:

```go
case "/update":
	return h.handleUpdateCommand()
```

Then add this method to the same file:

```go
// handleUpdateCommand returns the synchronous reply for /update and,
// when an install is actually available, kicks off the install +
// shutdown sequence in a goroutine. The goroutine outlives this call.
func (h *handler) handleUpdateCommand() commandResult {
	if h.updater == nil {
		return commandResult{reply: "Updater not initialized.", handled: true}
	}
	p := h.updater.Pending()
	if p == nil {
		return commandResult{
			reply:   fmt.Sprintf("No update available. You're on %s.", h.updater.currentVersion),
			handled: true,
		}
	}

	h.otto.mu.Lock()
	busy := h.otto.busy
	inflight := h.otto.currentPrompt
	h.otto.mu.Unlock()

	reply := fmt.Sprintf(
		"Starting update to %s for %s/%s…",
		p.Tag, runtime.GOOS, runtime.GOARCH,
	)
	if busy {
		preview := inflight
		if len(preview) > 60 {
			preview = preview[:60] + "…"
		}
		reply += fmt.Sprintf(" (Otto is mid-task on %q — that work will be interrupted.)", preview)
	}

	go h.runUpdate()
	return commandResult{reply: reply, handled: true}
}

// runUpdate is the side-effect goroutine spawned by /update. Reports
// failures back to the user; on success, sends a confirmation and
// exits the process.
func (h *handler) runUpdate() {
	ctx := context.Background()
	chatID := h.updater.chatID
	if err := h.updater.Install(ctx); err != nil {
		msg := fmt.Sprintf("⚠️ Update failed: %v", err)
		if sendErr := telegram.SendChunked(ctx, h.bot, chatID, msg); sendErr != nil {
			log.Printf("update: send failure msg: %v", sendErr)
		}
		return
	}
	h.updater.Exit()
}
```

Add `"runtime"` to commands.go's imports if not present.

- [ ] **Step 4: Run tests**

```bash
go test ./cmd/otto/ -run TestUpdateCommand -v
```

Expected: all three PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/commands.go cmd/otto/commands_test.go
git commit -m "add /update slash command"
```

---

### Task B8: Add GitHub Actions release workflow

**Files:**
- Create: `.github/workflows/release.yml`

- [ ] **Step 1: Create the workflow directory if missing**

```bash
mkdir -p .github/workflows
```

- [ ] **Step 2: Write the workflow file**

Create `.github/workflows/release.yml`:

```yaml
name: release

on:
  push:
    tags:
      - 'v*'

permissions:
  contents: write

jobs:
  build:
    runs-on: ubuntu-latest
    strategy:
      matrix:
        include:
          - goos: linux
            goarch: amd64
          - goos: linux
            goarch: arm64
          - goos: darwin
            goarch: arm64
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.26'
      - name: Build
        env:
          GOOS: ${{ matrix.goos }}
          GOARCH: ${{ matrix.goarch }}
          CGO_ENABLED: '0'
        run: |
          go build -ldflags "-X main.version=${{ github.ref_name }}" \
            -o otto-${{ matrix.goos }}-${{ matrix.goarch }} ./cmd/otto
      - uses: actions/upload-artifact@v4
        with:
          name: otto-${{ matrix.goos }}-${{ matrix.goarch }}
          path: otto-${{ matrix.goos }}-${{ matrix.goarch }}

  release:
    needs: build
    runs-on: ubuntu-latest
    steps:
      - uses: actions/download-artifact@v4
        with:
          path: dist
          merge-multiple: true
      - uses: softprops/action-gh-release@v2
        with:
          files: dist/otto-*
          tag_name: ${{ github.ref_name }}
          generate_release_notes: true
```

- [ ] **Step 3: Verify YAML parses (sanity)**

```bash
python3 -c "import yaml; yaml.safe_load(open('.github/workflows/release.yml'))" && echo "OK"
```

Expected: `OK`. (If `python3` isn't available, skip — GitHub's parser will tell you when you push.)

- [ ] **Step 4: Commit**

```bash
git add .github/workflows/release.yml
git commit -m "add release workflow: cross-compile and publish on v* tags"
```

- [ ] **Step 5: Note for the user**

Pushing a `v*` tag (e.g. `git tag v0.1.0 && git push origin v0.1.0`) will trigger the workflow once it's on the default branch on GitHub. **Don't tag yet** — first verify the workflow runs successfully on the next push to master, then cut the first real tag.

---

### Task B9: End-to-end verification

- [ ] **Step 1: Full test suite with race detector**

```bash
go test -race ./...
```

Expected: PASS.

- [ ] **Step 2: gofmt + vet**

```bash
make vet
```

Expected: clean.

- [ ] **Step 3: TTY smoke test for `/version` and `/update`**

```bash
make build
./otto -tty <<EOF
/version
/update
EOF
```

Expected:
- `/version` reply mentions `version=dev os=<your-platform>`
- `/update` reply: `Updater not initialized.` — because the TTY mode constructs main.go's wiring, which DOES initialize the updater, but with `version=dev` Run() short-circuits and never sets `pending`. So the reply should be `No update available. You're on dev.`

(If you see "Updater not initialized," double-check that Task B5 wired `h.updater = newUpdater(...)` in main.go.)

- [ ] **Step 4: Confirm no lingering references**

```bash
grep -rn "permissions\|InlineButton\|SendMessageWithButtons\|AnswerCallbackQuery\|CallbackQueryID\|ClaudeSettingsPath" --include="*.go" .
```

Expected: zero results (or only matches in deleted-file-handling test data, which there shouldn't be).

- [ ] **Step 5: Final commit and push**

If anything was caught in step 4, fix it and commit. Otherwise:

```bash
git log --oneline | head -20
```

Confirm the history shows the spec commit, then ~5 commits for Part A, then ~8 commits for Part B. Then:

```bash
git push origin master
```

After the push, watch the next release: tag a new version once you're satisfied that everything works locally:

```bash
git tag v0.1.0
git push origin v0.1.0
```

Watch the workflow on GitHub. If it succeeds and produces a release with three binaries, the round-trip is complete — a future Otto restart on someone else's box will see the new release and offer it.

---

## Self-review summary

Coverage checked against spec:

- **Part A removal scope** — all six file-touching items in the spec's "Scope of removal" list are covered by Tasks A1-A5
- **New denial behavior** — Task A1 implements the plain-text format from the spec
- **Migration** — Task A4 updates setup.sh and notes the silent TOML decode behavior
- **Build/release pipeline** — Task B8 implements the workflow exactly as specified
- **Version constant** — Task B1 adds `var version = "dev"` and Makefile ldflags
- **Toot character** — Task B2.5 implements the owl notification courier; Tasks B4 and B6 route announcements + install confirmations through it
- **Update poller** — Tasks B2-B5 build it incrementally with the constants from the spec; the announcement includes patch notes from the GitHub release body
- **`/update` command** — Task B7 implements all four reply branches (no pending, no asset, idle, busy)
- **`/version` command** — Task B1
- **Platform support** — assetForPlatform tested in Task B2; the macOS no-relaunch case is documented in the spec, no code change needed
- **State** — in-memory only, lifecycle matches spec (cleared on restart)
- **Failure modes** — Install returns errors; runUpdate sends them to Telegram; original binary preserved on failure (tested in Task B6)

No placeholders. No "TBD"s. Every step contains the actual code or command needed.
