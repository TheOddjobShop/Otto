# Otto Bot Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the Otto Telegram daemon — a single-user bot that long-polls Telegram, validates the sender against an allowlist, forwards text + images to Claude Code via subprocess (`claude -p ... --resume <session-id>`), and streams responses back. Working software at the end of this plan: a runnable `otto` binary that chats with you on Telegram, with persistent conversation memory.

**Architecture:** Single Go binary (`cmd/otto`) wiring four internal packages: `config` (TOML loader), `auth` (single-user allowlist), `telegram` (long-polling + chunked send + image download), `claude` (session ID persistence + stream-json parser + subprocess runner). Out of scope for this plan: Google MCP server (Plan 2), `setup.sh` and systemd packaging (Plan 3), `--google-auth` mode (Plan 2).

**Tech Stack:** Go 1.22+, `github.com/go-telegram-bot-api/telegram-bot-api/v5` (Telegram client), `github.com/BurntSushi/toml` (config), `github.com/google/uuid` (session IDs), standard library `os/exec` for Claude Code, `httptest` for Telegram tests.

---

## File Structure

```
.
├── cmd/otto/
│   ├── main.go              # Entry point, signal handling, wiring
│   ├── handler.go           # Per-message processing pipeline
│   └── commands.go          # Bot commands (/new, /whoami, /restart, /status)
├── internal/
│   ├── config/
│   │   ├── config.go        # Config struct + Load()
│   │   └── config_test.go
│   ├── auth/
│   │   ├── allowlist.go     # Allowlist.Allows(userID)
│   │   └── allowlist_test.go
│   ├── telegram/
│   │   ├── chunk.go         # ChunkMessage(text, limit) []string
│   │   ├── chunk_test.go
│   │   ├── client.go        # BotClient interface + real impl
│   │   ├── client_test.go
│   │   ├── send.go          # SendChunked(client, chatID, text)
│   │   ├── send_test.go
│   │   ├── download.go      # DownloadPhoto(client, fileID) ([]byte, string, error)
│   │   └── download_test.go
│   └── claude/
│       ├── session.go       # Session{ID, Path, Rotate}
│       ├── session_test.go
│       ├── event.go         # stream-json event types
│       ├── parser.go        # ParseEvents(io.Reader, chan<- Event)
│       ├── parser_test.go
│       ├── runner.go        # Runner interface + exec impl
│       └── runner_test.go
├── testdata/
│   └── fake-claude.sh       # Shell script emitting canned stream-json (test fixture)
├── Makefile
├── README.md
├── .gitignore
└── go.mod
```

**Module name:** `otto` (local module; rename to `github.com/justin06lee/...` only if/when pushing to a remote).

**Boundaries:**
- `internal/telegram` knows nothing about Claude or config — it's a pure I/O package.
- `internal/claude` knows nothing about Telegram — it's a pure subprocess wrapper.
- `cmd/otto` is the only place these wires meet.
- All packages depend on `internal/config` for typed config — but no package imports `cmd/otto`.

---

## Task 1: Initialize Go module and project scaffolding

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `README.md`
- Create: `Makefile`

- [ ] **Step 1: Initialize the module**

```bash
cd /Users/huiyunlee/Workspace/github.com/justin06lee/justinsoddjobshop/AbdurRazzaqBeta
go mod init otto
```

Expected: creates `go.mod` with `module otto` and a Go toolchain line.

- [ ] **Step 2: Create `.gitignore`**

```
# Binaries
/otto
/google-mcp
*.exe
*.test
*.out

# Local config / secrets — never commit
/config.toml
/client_secret.json
/google_token.json
/mcp.json

# Build artifacts
/dist/
/coverage.out
```

- [ ] **Step 3: Create minimal `README.md`**

```markdown
# Otto

Single-user Telegram bot wrapping Claude Code with MCP tools (Notion + Google APIs). Designed to run perpetually as a `systemd --user` service on an Arch Linux home server.

See `docs/superpowers/specs/2026-04-24-otto-design.md` for the design spec.

## Build

    make build

## Test

    make test

## Install

See Plan 3 (`setup.sh`).
```

- [ ] **Step 4: Create `Makefile`**

```makefile
.PHONY: build test test-integration vet clean

build:
	go build -o ./otto ./cmd/otto

test:
	go test ./...

test-integration:
	INTEGRATION=1 go test ./... -run Integration -v

vet:
	go vet ./...
	gofmt -l . | tee /dev/stderr | (! read)

clean:
	rm -f ./otto ./google-mcp coverage.out
```

- [ ] **Step 5: Verify scaffolding**

Run: `go vet ./... && go build ./...`
Expected: no output, exit 0. (Build does nothing yet — no Go files — but `go.mod` is valid.)

- [ ] **Step 6: Commit**

```bash
git add go.mod .gitignore README.md Makefile
git commit -m "chore: initialize Go module and scaffolding"
```

---

## Task 2: `internal/config` — TOML config loader

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`

- [ ] **Step 1: Add toml dependency**

Run: `go get github.com/BurntSushi/toml@latest`
Expected: `go.mod` and `go.sum` updated.

- [ ] **Step 2: Write the failing test**

`internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
telegram_bot_token = "tg-token"
telegram_allowed_user_id = 12345
anthropic_api_key = "sk-ant-test"
notion_api_key = "secret_test"
claude_binary_path = "/usr/bin/claude"
mcp_config_path = "/home/u/.config/otto/mcp.json"
session_id_path = "/home/u/.local/state/otto/session_id"
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TelegramBotToken != "tg-token" {
		t.Errorf("TelegramBotToken = %q", cfg.TelegramBotToken)
	}
	if cfg.TelegramAllowedUserID != 12345 {
		t.Errorf("TelegramAllowedUserID = %d", cfg.TelegramAllowedUserID)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	// Missing telegram_bot_token.
	contents := `
telegram_allowed_user_id = 12345
anthropic_api_key = "sk-ant-test"
notion_api_key = "secret_test"
claude_binary_path = "/usr/bin/claude"
mcp_config_path = "/tmp/mcp.json"
session_id_path = "/tmp/sid"
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected error for missing telegram_bot_token, got nil")
	}
}

func TestLoadFileNotFound(t *testing.T) {
	if _, err := Load("/nonexistent/config.toml"); err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

- [ ] **Step 3: Run tests — expect compile failure**

Run: `go test ./internal/config/...`
Expected: build error — `Load` is undefined.

- [ ] **Step 4: Implement `internal/config/config.go`**

```go
// Package config loads Otto's runtime configuration from a TOML file.
package config

import (
	"fmt"

	"github.com/BurntSushi/toml"
)

type Config struct {
	TelegramBotToken      string `toml:"telegram_bot_token"`
	TelegramAllowedUserID int64  `toml:"telegram_allowed_user_id"`
	AnthropicAPIKey       string `toml:"anthropic_api_key"`
	NotionAPIKey          string `toml:"notion_api_key"`
	ClaudeBinaryPath      string `toml:"claude_binary_path"`
	MCPConfigPath         string `toml:"mcp_config_path"`
	SessionIDPath         string `toml:"session_id_path"`
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	required := map[string]string{
		"telegram_bot_token": c.TelegramBotToken,
		"anthropic_api_key":  c.AnthropicAPIKey,
		"claude_binary_path": c.ClaudeBinaryPath,
		"mcp_config_path":    c.MCPConfigPath,
		"session_id_path":    c.SessionIDPath,
	}
	for k, v := range required {
		if v == "" {
			return fmt.Errorf("config: missing required field %q", k)
		}
	}
	if c.TelegramAllowedUserID == 0 {
		return fmt.Errorf("config: missing required field \"telegram_allowed_user_id\"")
	}
	return nil
}
```

- [ ] **Step 5: Run tests — expect pass**

Run: `go test ./internal/config/...`
Expected: `ok  otto/internal/config`.

- [ ] **Step 6: Commit**

```bash
git add internal/config go.mod go.sum
git commit -m "feat(config): add TOML config loader with validation"
```

---

## Task 3: `internal/auth` — single-user allowlist

**Files:**
- Create: `internal/auth/allowlist.go`
- Create: `internal/auth/allowlist_test.go`

- [ ] **Step 1: Write the failing test**

`internal/auth/allowlist_test.go`:

```go
package auth

import "testing"

func TestAllowsConfiguredUser(t *testing.T) {
	a := New(12345)
	if !a.Allows(12345) {
		t.Error("expected Allows(12345) = true")
	}
}

func TestRejectsOthers(t *testing.T) {
	a := New(12345)
	for _, id := range []int64{0, 1, 12344, 12346, -1} {
		if a.Allows(id) {
			t.Errorf("Allows(%d) = true, want false", id)
		}
	}
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `go test ./internal/auth/...`
Expected: build error — `New` undefined.

- [ ] **Step 3: Implement `internal/auth/allowlist.go`**

```go
// Package auth gates incoming Telegram messages by user ID.
package auth

type Allowlist struct {
	allowed int64
}

func New(allowedUserID int64) *Allowlist {
	return &Allowlist{allowed: allowedUserID}
}

func (a *Allowlist) Allows(userID int64) bool {
	return userID == a.allowed
}
```

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./internal/auth/...`
Expected: `ok  otto/internal/auth`.

- [ ] **Step 5: Commit**

```bash
git add internal/auth
git commit -m "feat(auth): add single-user allowlist"
```

---

## Task 4: `internal/telegram` — message chunking

**Files:**
- Create: `internal/telegram/chunk.go`
- Create: `internal/telegram/chunk_test.go`

- [ ] **Step 1: Write the failing test**

`internal/telegram/chunk_test.go`:

```go
package telegram

import (
	"strings"
	"testing"
)

func TestChunkUnderLimit(t *testing.T) {
	got := ChunkMessage("hello", 4096)
	if len(got) != 1 || got[0] != "hello" {
		t.Errorf("ChunkMessage = %q", got)
	}
}

func TestChunkEmptyString(t *testing.T) {
	got := ChunkMessage("", 4096)
	if len(got) != 0 {
		t.Errorf("ChunkMessage(\"\") = %v, want []", got)
	}
}

func TestChunkSplitsAtParagraphBoundary(t *testing.T) {
	// Two paragraphs, total > limit.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	got := ChunkMessage(a+"\n\n"+b, 4096)
	if len(got) != 2 {
		t.Fatalf("got %d chunks, want 2", len(got))
	}
	if got[0] != a {
		t.Errorf("chunk 0 = %q (truncated), want all 'a'", got[0][:20])
	}
	if got[1] != b {
		t.Errorf("chunk 1 = %q (truncated), want all 'b'", got[1][:20])
	}
}

func TestChunkSplitsAtNewlineFallback(t *testing.T) {
	// No paragraph break; uses single newlines.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	got := ChunkMessage(a+"\n"+b, 4096)
	if len(got) < 2 {
		t.Fatalf("got %d chunks, want >=2", len(got))
	}
}

func TestChunkVeryLongUnbrokenText(t *testing.T) {
	// No newlines at all — must fall back to hard char split.
	s := strings.Repeat("x", 10000)
	got := ChunkMessage(s, 4096)
	if len(got) < 3 {
		t.Fatalf("got %d chunks, want >=3", len(got))
	}
	for i, c := range got {
		if len(c) > 4096 {
			t.Errorf("chunk %d exceeds limit: %d", i, len(c))
		}
	}
	if strings.Join(got, "") != s {
		t.Error("rejoined chunks don't match original")
	}
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `go test ./internal/telegram/...`
Expected: `ChunkMessage` undefined.

- [ ] **Step 3: Implement `internal/telegram/chunk.go`**

```go
// Package telegram implements the Telegram Bot API client used by Otto.
package telegram

import "strings"

// ChunkMessage splits text into pieces at most `limit` runes each, preferring
// paragraph (\n\n) boundaries, then newline boundaries, then hard char splits.
func ChunkMessage(text string, limit int) []string {
	if text == "" {
		return nil
	}
	if len(text) <= limit {
		return []string{text}
	}
	if chunks := splitOn(text, "\n\n", limit); chunks != nil {
		return chunks
	}
	if chunks := splitOn(text, "\n", limit); chunks != nil {
		return chunks
	}
	return hardSplit(text, limit)
}

func splitOn(text, sep string, limit int) []string {
	parts := strings.Split(text, sep)
	if len(parts) == 1 {
		return nil
	}
	var out []string
	var cur strings.Builder
	for _, p := range parts {
		// If a single part exceeds limit, this strategy can't help.
		if len(p) > limit {
			return nil
		}
		// +len(sep) for the joining sep we'd add if cur is non-empty.
		addLen := len(p)
		if cur.Len() > 0 {
			addLen += len(sep)
		}
		if cur.Len()+addLen > limit {
			out = append(out, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteString(sep)
		}
		cur.WriteString(p)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

func hardSplit(text string, limit int) []string {
	var out []string
	for len(text) > limit {
		out = append(out, text[:limit])
		text = text[limit:]
	}
	if len(text) > 0 {
		out = append(out, text)
	}
	return out
}
```

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./internal/telegram/...`
Expected: `ok  otto/internal/telegram`.

- [ ] **Step 5: Commit**

```bash
git add internal/telegram
git commit -m "feat(telegram): add message chunking respecting 4096 limit"
```

---

## Task 5: `internal/telegram` — `BotClient` interface and real implementation

**Files:**
- Create: `internal/telegram/client.go`
- Create: `internal/telegram/client_test.go`

- [ ] **Step 1: Add Telegram library**

Run: `go get github.com/go-telegram-bot-api/telegram-bot-api/v5@latest`
Expected: `go.mod` updated.

- [ ] **Step 2: Write the failing test**

`internal/telegram/client_test.go`:

```go
package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRealClientSendMessageHitsAPI(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 1, "chat": map[string]any{"id": 99}, "date": 0},
		})
	}))
	defer srv.Close()

	c, err := NewBotClient("test-token", srv.URL+"/bot%s/%s")
	if err != nil {
		t.Fatalf("NewBotClient: %v", err)
	}

	if err := c.SendMessage(context.Background(), 99, "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if !strings.Contains(gotPath, "sendMessage") {
		t.Errorf("path = %q, want contains sendMessage", gotPath)
	}
	if !strings.Contains(gotBody, "hi") {
		t.Errorf("body = %q, want contains 'hi'", gotBody)
	}
}
```

- [ ] **Step 3: Run tests — expect compile failure**

Run: `go test ./internal/telegram/... -run TestRealClient`
Expected: `NewBotClient` undefined.

- [ ] **Step 4: Implement `internal/telegram/client.go`**

```go
package telegram

import (
	"context"
	"fmt"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Update is the slice of a Telegram update Otto cares about.
type Update struct {
	UpdateID int
	ChatID   int64
	UserID   int64
	Text     string
	PhotoIDs []string // largest-size photo file_id per photo, if any
}

// BotClient is the surface of Telegram operations Otto needs. Defined as an
// interface so cmd/otto can be unit-tested with a fake.
type BotClient interface {
	GetUpdates(ctx context.Context, offset int) ([]Update, error)
	SendMessage(ctx context.Context, chatID int64, text string) error
	DownloadFile(ctx context.Context, fileID string) ([]byte, string, error)
}

type realClient struct {
	api *tgbotapi.BotAPI
}

// NewBotClient returns a real Telegram client. apiURLTemplate is the format
// string used by tgbotapi (e.g. "https://api.telegram.org/bot%s/%s"); pass
// httptest.NewServer URL + "/bot%s/%s" in tests.
func NewBotClient(token, apiURLTemplate string) (BotClient, error) {
	api, err := tgbotapi.NewBotAPIWithAPIEndpoint(token, apiURLTemplate)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}
	api.Debug = false
	return &realClient{api: api}, nil
}

func (c *realClient) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	cfg := tgbotapi.NewUpdate(offset)
	cfg.Timeout = 30
	updates, err := c.api.GetUpdates(cfg)
	if err != nil {
		return nil, fmt.Errorf("telegram: get updates: %w", err)
	}
	out := make([]Update, 0, len(updates))
	for _, u := range updates {
		out = append(out, fromTGUpdate(u))
	}
	return out, nil
}

func (c *realClient) SendMessage(ctx context.Context, chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	if _, err := c.api.Send(msg); err != nil {
		return fmt.Errorf("telegram: send: %w", err)
	}
	return nil
}

func (c *realClient) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	url, err := c.api.GetFileDirectURL(fileID)
	if err != nil {
		return nil, "", fmt.Errorf("telegram: get file url: %w", err)
	}
	return downloadURL(ctx, url)
}

func fromTGUpdate(u tgbotapi.Update) Update {
	out := Update{UpdateID: u.UpdateID}
	if u.Message == nil {
		return out
	}
	out.ChatID = u.Message.Chat.ID
	if u.Message.From != nil {
		out.UserID = u.Message.From.ID
	}
	out.Text = u.Message.Text
	if u.Message.Caption != "" && out.Text == "" {
		out.Text = u.Message.Caption
	}
	if len(u.Message.Photo) > 0 {
		// Telegram returns multiple sizes; pick the largest.
		largest := u.Message.Photo[len(u.Message.Photo)-1]
		out.PhotoIDs = []string{largest.FileID}
	}
	return out
}
```

The `downloadURL` helper is added in Task 7. For now, stub it to keep the package compiling:

Add to `internal/telegram/client.go`:

```go
import (
	"context"
	"fmt"
	"io"
	"net/http"
)

func downloadURL(ctx context.Context, url string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", fmt.Errorf("telegram: download status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err
	}
	return body, resp.Header.Get("Content-Type"), nil
}
```

- [ ] **Step 5: Run tests — expect pass**

Run: `go test ./internal/telegram/...`
Expected: all tests pass including chunk tests and the new `TestRealClientSendMessageHitsAPI`.

- [ ] **Step 6: Commit**

```bash
git add internal/telegram go.mod go.sum
git commit -m "feat(telegram): add BotClient interface and real impl"
```

---

## Task 6: `internal/telegram` — chunked send wrapper

**Files:**
- Create: `internal/telegram/send.go`
- Create: `internal/telegram/send_test.go`

- [ ] **Step 1: Write the failing test**

`internal/telegram/send_test.go`:

```go
package telegram

import (
	"context"
	"strings"
	"testing"
)

type fakeClient struct {
	sent []string
}

func (f *fakeClient) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	return nil, nil
}
func (f *fakeClient) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.sent = append(f.sent, text)
	return nil
}
func (f *fakeClient) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return nil, "", nil
}

func TestSendChunkedShortMessage(t *testing.T) {
	f := &fakeClient{}
	if err := SendChunked(context.Background(), f, 1, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 1 || f.sent[0] != "hello" {
		t.Errorf("sent = %v", f.sent)
	}
}

func TestSendChunkedLongMessage(t *testing.T) {
	f := &fakeClient{}
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	if err := SendChunked(context.Background(), f, 1, a+"\n\n"+b); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 2 {
		t.Fatalf("got %d sends, want 2", len(f.sent))
	}
}

func TestSendChunkedEmpty(t *testing.T) {
	f := &fakeClient{}
	if err := SendChunked(context.Background(), f, 1, ""); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 0 {
		t.Errorf("sent = %v, want []", f.sent)
	}
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `go test ./internal/telegram/... -run SendChunked`
Expected: `SendChunked` undefined.

- [ ] **Step 3: Implement `internal/telegram/send.go`**

```go
package telegram

import "context"

// MaxMessageLen is Telegram's per-message limit.
const MaxMessageLen = 4096

// SendChunked sends `text` to chatID, splitting into multiple messages if it
// exceeds MaxMessageLen. Empty text is a no-op.
func SendChunked(ctx context.Context, c BotClient, chatID int64, text string) error {
	for _, chunk := range ChunkMessage(text, MaxMessageLen) {
		if err := c.SendMessage(ctx, chatID, chunk); err != nil {
			return err
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./internal/telegram/...`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/telegram/send.go internal/telegram/send_test.go
git commit -m "feat(telegram): add chunked send helper"
```

---

## Task 7: `internal/telegram` — image download to temp file

**Files:**
- Create: `internal/telegram/download.go`
- Create: `internal/telegram/download_test.go`

- [ ] **Step 1: Write the failing test**

`internal/telegram/download_test.go`:

```go
package telegram

import (
	"context"
	"os"
	"strings"
	"testing"
)

type fakeDownloader struct {
	data        []byte
	contentType string
}

func (f *fakeDownloader) GetUpdates(ctx context.Context, offset int) ([]Update, error) {
	return nil, nil
}
func (f *fakeDownloader) SendMessage(ctx context.Context, chatID int64, text string) error {
	return nil
}
func (f *fakeDownloader) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return f.data, f.contentType, nil
}

func TestDownloadPhotoToTemp(t *testing.T) {
	f := &fakeDownloader{data: []byte("\x89PNG\x0D\x0A\x1A\x0A"), contentType: "image/png"}
	dir := t.TempDir()

	path, err := DownloadPhotoToTemp(context.Background(), f, "FILE-ABC", dir)
	if err != nil {
		t.Fatalf("DownloadPhotoToTemp: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("path %q not under tempdir %q", path, dir)
	}
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("path %q missing .png extension", path)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(f.data) {
		t.Errorf("contents = %q, want %q", got, f.data)
	}
}

func TestDownloadPhotoUnknownContentType(t *testing.T) {
	f := &fakeDownloader{data: []byte("data"), contentType: ""}
	dir := t.TempDir()

	path, err := DownloadPhotoToTemp(context.Background(), f, "FILE-ABC", dir)
	if err != nil {
		t.Fatalf("DownloadPhotoToTemp: %v", err)
	}
	if !strings.HasSuffix(path, ".bin") {
		t.Errorf("path %q missing .bin fallback extension", path)
	}
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `go test ./internal/telegram/... -run DownloadPhoto`
Expected: `DownloadPhotoToTemp` undefined.

- [ ] **Step 3: Implement `internal/telegram/download.go`**

```go
package telegram

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func extensionFor(contentType string) string {
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".bin"
}

// DownloadPhotoToTemp downloads the file by ID and writes it to a uniquely
// named file under dir, returning the absolute path. Caller is responsible
// for cleanup.
func DownloadPhotoToTemp(ctx context.Context, c BotClient, fileID, dir string) (string, error) {
	data, contentType, err := c.DownloadFile(ctx, fileID)
	if err != nil {
		return "", fmt.Errorf("telegram: download %s: %w", fileID, err)
	}
	ext := extensionFor(contentType)
	f, err := os.CreateTemp(dir, "tgphoto-*"+ext)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	abs, _ := filepath.Abs(f.Name())
	return abs, nil
}
```

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./internal/telegram/...`
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add internal/telegram/download.go internal/telegram/download_test.go
git commit -m "feat(telegram): add photo download to temp file"
```

---

## Task 8: `internal/claude` — session ID persistence

**Files:**
- Create: `internal/claude/session.go`
- Create: `internal/claude/session_test.go`

- [ ] **Step 1: Add uuid dependency**

Run: `go get github.com/google/uuid@latest`
Expected: `go.mod` updated.

- [ ] **Step 2: Write the failing test**

`internal/claude/session_test.go`:

```go
package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionLoadsExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	if err := os.WriteFile(path, []byte("abc-123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := LoadOrCreateSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "abc-123" {
		t.Errorf("ID = %q, want abc-123", s.ID())
	}
}

func TestSessionGeneratesIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "session_id")
	s, err := LoadOrCreateSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() == "" {
		t.Error("ID is empty")
	}
	// Persisted to disk.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != s.ID() {
		t.Errorf("file contents %q != session ID %q", got, s.ID())
	}
}

func TestSessionRotateChangesID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_id")
	s, err := LoadOrCreateSession(path)
	if err != nil {
		t.Fatal(err)
	}
	old := s.ID()
	if err := s.Rotate(); err != nil {
		t.Fatal(err)
	}
	if s.ID() == old {
		t.Error("ID did not change after Rotate")
	}
	// New ID persisted.
	got, _ := os.ReadFile(path)
	if strings.TrimSpace(string(got)) != s.ID() {
		t.Errorf("file %q != ID %q", got, s.ID())
	}
}
```

- [ ] **Step 3: Run tests — expect compile failure**

Run: `go test ./internal/claude/...`
Expected: `LoadOrCreateSession` undefined.

- [ ] **Step 4: Implement `internal/claude/session.go`**

```go
// Package claude wraps the Claude Code CLI as a subprocess and parses its
// stream-json output.
package claude

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Session holds the current Claude Code session ID, persisted at Path so it
// survives Otto restarts.
type Session struct {
	mu   sync.RWMutex
	id   string
	path string
}

// LoadOrCreateSession reads the session ID from path, generating and writing
// a new one if the file is missing.
func LoadOrCreateSession(path string) (*Session, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("claude: ensure session dir: %w", err)
	}
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			return &Session{id: id, path: path}, nil
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("claude: read session: %w", err)
	}
	id := uuid.NewString()
	if err := writeSession(path, id); err != nil {
		return nil, err
	}
	return &Session{id: id, path: path}, nil
}

func (s *Session) ID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.id
}

func (s *Session) Path() string { return s.path }

// Rotate generates a new session ID and persists it.
func (s *Session) Rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := uuid.NewString()
	if err := writeSession(s.path, id); err != nil {
		return err
	}
	s.id = id
	return nil
}

func writeSession(path, id string) error {
	if err := os.WriteFile(path, []byte(id+"\n"), 0600); err != nil {
		return fmt.Errorf("claude: write session: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run tests — expect pass**

Run: `go test ./internal/claude/...`
Expected: `ok  otto/internal/claude`.

- [ ] **Step 6: Commit**

```bash
git add internal/claude/session.go internal/claude/session_test.go go.mod go.sum
git commit -m "feat(claude): add persistent session ID with rotation"
```

---

## Task 9: `internal/claude` — stream-json parser

**Files:**
- Create: `internal/claude/event.go`
- Create: `internal/claude/parser.go`
- Create: `internal/claude/parser_test.go`

- [ ] **Step 1: Write the failing test**

`internal/claude/parser_test.go`:

```go
package claude

import (
	"context"
	"strings"
	"testing"
)

func TestParseStreamAccumulatesAssistantText(t *testing.T) {
	// Two assistant text deltas, then a result event.
	in := strings.NewReader(`
{"type":"assistant","message":{"content":[{"type":"text","text":"Hello "}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"world."}]}}
{"type":"result","subtype":"success"}
`)

	events := make(chan Event, 16)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)

	var assistantText strings.Builder
	sawResult := false
	for ev := range events {
		switch e := ev.(type) {
		case AssistantTextEvent:
			assistantText.WriteString(e.Text)
		case ResultEvent:
			sawResult = true
		}
	}
	if got := assistantText.String(); got != "Hello world." {
		t.Errorf("assistant text = %q, want %q", got, "Hello world.")
	}
	if !sawResult {
		t.Error("did not see ResultEvent")
	}
}

func TestParseStreamIgnoresUnknownTypes(t *testing.T) {
	in := strings.NewReader(`
{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
`)
	events := make(chan Event, 8)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	var saw []Event
	for ev := range events {
		saw = append(saw, ev)
	}
	if len(saw) != 1 {
		t.Errorf("got %d events, want 1 (only assistant text)", len(saw))
	}
}

func TestParseStreamSkipsBlankLines(t *testing.T) {
	in := strings.NewReader("\n\n\n")
	events := make(chan Event, 1)
	if err := ParseStream(context.Background(), in, events); err != nil {
		t.Fatalf("ParseStream: %v", err)
	}
	close(events)
	for range events {
		t.Fatal("expected no events")
	}
}

func TestParseStreamMalformedLineReturnsError(t *testing.T) {
	in := strings.NewReader(`{not valid json`)
	events := make(chan Event, 1)
	if err := ParseStream(context.Background(), in, events); err == nil {
		t.Fatal("expected error, got nil")
	}
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `go test ./internal/claude/... -run ParseStream`
Expected: `ParseStream`, `Event`, `AssistantTextEvent`, `ResultEvent` undefined.

- [ ] **Step 3: Implement `internal/claude/event.go`**

```go
package claude

// Event is the discriminated union emitted by ParseStream.
type Event interface{ isEvent() }

type AssistantTextEvent struct{ Text string }

func (AssistantTextEvent) isEvent() {}

type ResultEvent struct {
	Subtype string
	Error   string // populated when Subtype != "success"
}

func (ResultEvent) isEvent() {}
```

- [ ] **Step 4: Implement `internal/claude/parser.go`**

```go
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
)

// rawMessage matches the subset of stream-json events we use.
type rawMessage struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Error   string `json:"error"`
	Message struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// ParseStream reads newline-delimited JSON from r and forwards interpreted
// events to events. Returns on EOF, ctx cancel, or first parse error.
func ParseStream(ctx context.Context, r io.Reader, events chan<- Event) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw rawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			return fmt.Errorf("claude: parse stream-json: %w", err)
		}
		switch raw.Type {
		case "assistant":
			for _, c := range raw.Message.Content {
				if c.Type == "text" && c.Text != "" {
					events <- AssistantTextEvent{Text: c.Text}
				}
			}
		case "result":
			events <- ResultEvent{Subtype: raw.Subtype, Error: raw.Error}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("claude: scan stream: %w", err)
	}
	return nil
}
```

- [ ] **Step 5: Run tests — expect pass**

Run: `go test ./internal/claude/...`
Expected: all pass.

- [ ] **Step 6: Commit**

```bash
git add internal/claude/event.go internal/claude/parser.go internal/claude/parser_test.go
git commit -m "feat(claude): add stream-json parser"
```

---

## Task 10: `internal/claude` — subprocess Runner

**Files:**
- Create: `internal/claude/runner.go`
- Create: `internal/claude/runner_test.go`
- Create: `testdata/fake-claude.sh`

- [ ] **Step 1: Create the test fixture**

`testdata/fake-claude.sh`:

```bash
#!/usr/bin/env bash
# Fake Claude binary for tests. Echoes a canned stream-json response.
# Reads any stdin to simulate prompt input.
set -e

# Optionally vary output by env var.
case "${FAKE_CLAUDE_MODE:-ok}" in
  ok)
    cat <<'JSON'
{"type":"assistant","message":{"content":[{"type":"text","text":"hello "}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"world"}]}}
{"type":"result","subtype":"success"}
JSON
    ;;
  fail)
    echo "claude: fake error" 1>&2
    exit 1
    ;;
  hang)
    sleep 30
    ;;
esac
```

Then make it executable:

```bash
chmod +x testdata/fake-claude.sh
```

- [ ] **Step 2: Write the failing test**

`internal/claude/runner_test.go`:

```go
package claude

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeClaudePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunnerStreamsAssistantText(t *testing.T) {
	r := NewExecRunner(fakeClaudePath(t), "fake-key", "/tmp/mcp.json")
	events := make(chan Event, 16)
	err := r.Run(context.Background(), RunArgs{
		Prompt:    "hi",
		SessionID: "abc",
		Events:    events,
	})
	close(events)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var text strings.Builder
	for ev := range events {
		if t, ok := ev.(AssistantTextEvent); ok {
			text.WriteString(t.Text)
		}
	}
	if text.String() != "hello world" {
		t.Errorf("text = %q, want %q", text.String(), "hello world")
	}
}

func TestRunnerSurfacesNonZeroExit(t *testing.T) {
	r := NewExecRunner(fakeClaudePath(t), "fake-key", "/tmp/mcp.json")
	r = r.WithEnv(map[string]string{"FAKE_CLAUDE_MODE": "fail"})
	events := make(chan Event, 4)
	err := r.Run(context.Background(), RunArgs{Prompt: "hi", SessionID: "abc", Events: events})
	close(events)
	if err == nil {
		t.Fatal("expected error from non-zero exit")
	}
	if !strings.Contains(err.Error(), "fake error") {
		t.Errorf("error %q does not contain stderr", err)
	}
}

func TestRunnerRespectsContextTimeout(t *testing.T) {
	r := NewExecRunner(fakeClaudePath(t), "fake-key", "/tmp/mcp.json")
	r = r.WithEnv(map[string]string{"FAKE_CLAUDE_MODE": "hang"})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	events := make(chan Event, 4)
	start := time.Now()
	err := r.Run(ctx, RunArgs{Prompt: "hi", SessionID: "abc", Events: events})
	close(events)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Run took too long: %v", time.Since(start))
	}
}
```

- [ ] **Step 3: Run tests — expect compile failure**

Run: `go test ./internal/claude/... -run Runner`
Expected: `NewExecRunner`, `RunArgs` undefined.

- [ ] **Step 4: Implement `internal/claude/runner.go`**

```go
package claude

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
)

// RunArgs is the per-call input to Runner.Run.
type RunArgs struct {
	Prompt     string
	SessionID  string
	ImagePaths []string  // optional; appended to prompt as path references
	Events     chan<- Event
}

// Runner runs Claude Code subprocesses.
type Runner interface {
	Run(ctx context.Context, args RunArgs) error
	WithEnv(extra map[string]string) Runner
}

type execRunner struct {
	binary        string
	apiKey        string
	mcpConfigPath string
	extraEnv      map[string]string
}

// NewExecRunner returns a Runner that invokes the given Claude Code binary.
func NewExecRunner(binary, apiKey, mcpConfigPath string) Runner {
	return &execRunner{binary: binary, apiKey: apiKey, mcpConfigPath: mcpConfigPath}
}

func (r *execRunner) WithEnv(extra map[string]string) Runner {
	merged := map[string]string{}
	for k, v := range r.extraEnv {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return &execRunner{
		binary:        r.binary,
		apiKey:        r.apiKey,
		mcpConfigPath: r.mcpConfigPath,
		extraEnv:      merged,
	}
}

func (r *execRunner) Run(ctx context.Context, args RunArgs) error {
	prompt := args.Prompt
	for _, p := range args.ImagePaths {
		// Verify exact CLI syntax against the installed Claude Code version
		// during integration testing; this @path form is the documented
		// reference syntax at the time of writing.
		prompt += " @" + p
	}

	cmd := exec.CommandContext(ctx, r.binary,
		"-p", prompt,
		"--resume", args.SessionID,
		"--mcp-config", r.mcpConfigPath,
		"--output-format", "stream-json",
		"--verbose",
	)
	cmd.Env = append(os.Environ(), "ANTHROPIC_API_KEY="+r.apiKey)
	for k, v := range r.extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Put process in own group so we can kill children on timeout.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claude: start: %w", err)
	}

	parseDone := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		parseDone <- ParseStream(ctx, stdout, args.Events)
	}()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	select {
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-waitDone
		wg.Wait()
		_ = drain(stdout)
		return ctx.Err()
	case waitErr := <-waitDone:
		wg.Wait()
		parseErr := <-parseDone
		if waitErr != nil {
			return fmt.Errorf("claude: %w: %s", waitErr, strings.TrimSpace(stderr.String()))
		}
		if parseErr != nil {
			return parseErr
		}
		return nil
	}
}

func drain(r io.Reader) error {
	_, err := io.Copy(io.Discard, r)
	return err
}
```

- [ ] **Step 5: Run tests — expect pass**

Run: `go test ./internal/claude/...`
Expected: all pass. (Note: `TestRunnerRespectsContextTimeout` may take ~200ms.)

- [ ] **Step 6: Commit**

```bash
git add internal/claude/runner.go internal/claude/runner_test.go testdata/fake-claude.sh
git commit -m "feat(claude): add subprocess Runner with timeout and stderr capture"
```

---

## Task 11: `cmd/otto` — main entry, signal handling, polling loop scaffold

**Files:**
- Create: `cmd/otto/main.go`

- [ ] **Step 1: Write `cmd/otto/main.go`**

```go
// Otto is a single-user Telegram bot that proxies messages to Claude Code.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/config"
	"otto/internal/telegram"
)

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.toml")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	allow := auth.New(cfg.TelegramAllowedUserID)

	bot, err := telegram.NewBotClient(cfg.TelegramBotToken, "https://api.telegram.org/bot%s/%s")
	if err != nil {
		log.Fatalf("telegram: %v", err)
	}

	session, err := claude.LoadOrCreateSession(cfg.SessionIDPath)
	if err != nil {
		log.Fatalf("claude session: %v", err)
	}

	runner := claude.NewExecRunner(cfg.ClaudeBinaryPath, cfg.AnthropicAPIKey, cfg.MCPConfigPath)

	h := &handler{
		bot:     bot,
		allow:   allow,
		session: session,
		runner:  runner,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("otto: received %s, shutting down", s)
		cancel()
	}()

	log.Printf("otto: starting; session=%s allowed_user=%d", session.ID(), cfg.TelegramAllowedUserID)
	if err := h.runPollingLoop(ctx); err != nil && err != context.Canceled {
		log.Fatalf("polling loop: %v", err)
	}
	log.Printf("otto: stopped")
}

func defaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "config.toml"
	}
	return home + "/.config/otto/config.toml"
}
```

- [ ] **Step 2: Verify it builds (handler not yet defined; skip build until next task)**

Skip — `handler` is added in Task 12.

- [ ] **Step 3: Commit (deferred)**

Combine commit with Task 12, since `main.go` references `handler` which lives there. Don't commit yet.

---

## Task 12: `cmd/otto` — message handler and polling loop

**Files:**
- Create: `cmd/otto/handler.go`
- Create: `cmd/otto/handler_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/otto/handler_test.go`:

```go
package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/telegram"
)

type fakeBot struct {
	updates [][]telegram.Update
	idx     int
	sent    []sentMsg
}

type sentMsg struct {
	chatID int64
	text   string
}

func (f *fakeBot) GetUpdates(ctx context.Context, offset int) ([]telegram.Update, error) {
	if f.idx >= len(f.updates) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	u := f.updates[f.idx]
	f.idx++
	return u, nil
}

func (f *fakeBot) SendMessage(ctx context.Context, chatID int64, text string) error {
	f.sent = append(f.sent, sentMsg{chatID, text})
	return nil
}

func (f *fakeBot) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return nil, "", errors.New("not used")
}

type fakeRunner struct {
	respond string
	failErr error
	called  []claude.RunArgs
}

func (r *fakeRunner) Run(ctx context.Context, args claude.RunArgs) error {
	r.called = append(r.called, args)
	if r.failErr != nil {
		return r.failErr
	}
	args.Events <- claude.AssistantTextEvent{Text: r.respond}
	args.Events <- claude.ResultEvent{Subtype: "success"}
	return nil
}

func (r *fakeRunner) WithEnv(extra map[string]string) claude.Runner { return r }

func newTestHandler(t *testing.T, bot telegram.BotClient, runner claude.Runner) *handler {
	t.Helper()
	dir := t.TempDir()
	sess, err := claude.LoadOrCreateSession(filepath.Join(dir, "sid"))
	if err != nil {
		t.Fatal(err)
	}
	return &handler{
		bot:     bot,
		allow:   auth.New(99),
		session: sess,
		runner:  runner,
	}
}

// runForBriefWindow runs the polling loop for a short, deterministic window,
// then returns. The fakeBot blocks on ctx.Done() once updates are exhausted,
// so the loop returns as soon as ctx expires.
func runForBriefWindow(t *testing.T, h *handler) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = h.runPollingLoop(ctx)
}

func TestHandlerForwardsTextMessage(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{respond: "hello!"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner called %d times, want 1", len(runner.called))
	}
	if runner.called[0].Prompt != "hi" {
		t.Errorf("prompt = %q", runner.called[0].Prompt)
	}
	if len(bot.sent) != 1 || bot.sent[0].text != "hello!" {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestHandlerDropsNonAllowlistedUser(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 7, Text: "hi"}}},
	}
	runner := &fakeRunner{respond: "should not run"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 0 {
		t.Errorf("runner was called for non-allowlisted user")
	}
	if len(bot.sent) != 0 {
		t.Errorf("bot sent message to non-allowlisted user")
	}
}

func TestHandlerSendsErrorOnRunnerFailure(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "hi"}}},
	}
	runner := &fakeRunner{failErr: errors.New("boom")}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(bot.sent) != 1 {
		t.Fatalf("sent = %+v", bot.sent)
	}
	if !strings.Contains(bot.sent[0].text, "boom") {
		t.Errorf("error message = %q, missing 'boom'", bot.sent[0].text)
	}
}
```

- [ ] **Step 2: Run tests — expect compile failure**

Run: `go test ./cmd/otto/...`
Expected: `handler` undefined.

- [ ] **Step 3: Implement `cmd/otto/handler.go`**

```go
package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/telegram"
)

const (
	pollErrorBaseBackoff = time.Second
	pollErrorMaxBackoff  = time.Minute
	claudeCallTimeout    = 5 * time.Minute
)

type handler struct {
	bot     telegram.BotClient
	allow   *auth.Allowlist
	session *claude.Session
	runner  claude.Runner

	mu sync.Mutex // serializes Claude calls per design spec
}

func (h *handler) runPollingLoop(ctx context.Context) error {
	offset := 0
	backoff := pollErrorBaseBackoff
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		updates, err := h.bot.GetUpdates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("polling error: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > pollErrorMaxBackoff {
				backoff = pollErrorMaxBackoff
			}
			continue
		}
		backoff = pollErrorBaseBackoff
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			h.dispatch(ctx, u)
		}
	}
}

func (h *handler) dispatch(ctx context.Context, u telegram.Update) {
	if !h.allow.Allows(u.UserID) {
		log.Printf("dropping message from non-allowlisted user %d", u.UserID)
		return
	}
	if strings.TrimSpace(u.Text) == "" && len(u.PhotoIDs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handleMessage(ctx, u)
}

func (h *handler) handleMessage(ctx context.Context, u telegram.Update) {
	callCtx, cancel := context.WithTimeout(ctx, claudeCallTimeout)
	defer cancel()

	events := make(chan claude.Event, 64)
	doneParsing := make(chan struct{})
	var assistantText strings.Builder

	go func() {
		defer close(doneParsing)
		for ev := range events {
			if t, ok := ev.(claude.AssistantTextEvent); ok {
				assistantText.WriteString(t.Text)
			}
		}
	}()

	err := h.runner.Run(callCtx, claude.RunArgs{
		Prompt:    u.Text,
		SessionID: h.session.ID(),
		Events:    events,
	})
	close(events)
	<-doneParsing

	if err != nil {
		_ = telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ Claude error: %s", err))
		return
	}
	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		out = "(no response)"
	}
	if err := telegram.SendChunked(ctx, h.bot, u.ChatID, out); err != nil {
		log.Printf("send error: %v", err)
	}
}
```

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./cmd/otto/...`
Expected: all three tests pass.

- [ ] **Step 5: Build the binary**

Run: `go build -o ./otto ./cmd/otto`
Expected: produces `./otto` with no errors.

- [ ] **Step 6: Commit**

```bash
git add cmd/otto/main.go cmd/otto/handler.go cmd/otto/handler_test.go
git commit -m "feat(cmd/otto): add daemon entrypoint and message handler"
```

---

## Task 13: `cmd/otto` — bot commands (`/new`, `/whoami`, `/restart`, `/status`)

**Files:**
- Create: `cmd/otto/commands.go`
- Modify: `cmd/otto/handler.go` (intercept commands before dispatching to Claude)
- Create: `cmd/otto/commands_test.go`

- [ ] **Step 1: Write the failing test**

`cmd/otto/commands_test.go`:

```go
// This file is in the same package as handler_test.go and reuses fakeBot,
// fakeRunner, newTestHandler, and runForBriefWindow from there.

package main

import (
	"strings"
	"testing"

	"otto/internal/telegram"
)

func TestCommandNewRotatesSession(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "/new"}}},
	}
	runner := &fakeRunner{respond: "ignored"}
	h := newTestHandler(t, bot, runner)

	old := h.session.ID()
	runForBriefWindow(t, h)

	if h.session.ID() == old {
		t.Error("session ID did not change")
	}
	if len(runner.called) != 0 {
		t.Errorf("runner called %d times for /new, want 0", len(runner.called))
	}
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "new session") {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestCommandWhoami(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "/whoami"}}},
	}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 0 {
		t.Errorf("runner called for /whoami")
	}
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "session=") {
		t.Errorf("sent = %+v", bot.sent)
	}
}

func TestCommandStatus(t *testing.T) {
	bot := &fakeBot{
		updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "/status"}}},
	}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 0 {
		t.Errorf("runner called for /status")
	}
	if len(bot.sent) != 1 || !strings.Contains(bot.sent[0].text, "uptime") {
		t.Errorf("sent = %+v", bot.sent)
	}
}
```

- [ ] **Step 2: Implement `cmd/otto/commands.go`**

```go
package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"otto/internal/telegram"
)

type commandResult struct {
	reply   string
	handled bool
}

func (h *handler) tryCommand(ctx context.Context, u telegram.Update) commandResult {
	text := strings.TrimSpace(u.Text)
	if !strings.HasPrefix(text, "/") {
		return commandResult{}
	}
	parts := strings.Fields(text)
	switch parts[0] {
	case "/new":
		if err := h.session.Rotate(); err != nil {
			return commandResult{reply: fmt.Sprintf("⚠️ rotate failed: %v", err), handled: true}
		}
		return commandResult{reply: fmt.Sprintf("✨ Started new session: %s", h.session.ID()), handled: true}
	case "/whoami":
		return commandResult{
			reply:   fmt.Sprintf("user=%d session=%s", u.UserID, h.session.ID()),
			handled: true,
		}
	case "/restart":
		// In-flight Claude calls hold h.mu; tryCommand runs under h.mu, so by
		// the time we get here any in-flight call has already returned. The
		// command exists for symmetry with the design and as a no-op ack.
		return commandResult{reply: "🔄 No in-flight call. (Send /new to start fresh.)", handled: true}
	case "/status":
		return commandResult{
			reply:   fmt.Sprintf("uptime=%s session=%s", time.Since(h.startedAt).Round(time.Second), h.session.ID()),
			handled: true,
		}
	}
	return commandResult{}
}
```

- [ ] **Step 3: Modify `cmd/otto/handler.go` to intercept commands**

Add `startedAt time.Time` field to the `handler` struct and initialize it in `cmd/otto/main.go` (`h := &handler{ ... startedAt: time.Now()}`).

In `dispatch()`, intercept commands before locking the runner mutex would block them too long. Update:

```go
func (h *handler) dispatch(ctx context.Context, u telegram.Update) {
	if !h.allow.Allows(u.UserID) {
		log.Printf("dropping message from non-allowlisted user %d", u.UserID)
		return
	}
	if strings.TrimSpace(u.Text) == "" && len(u.PhotoIDs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if cmd := h.tryCommand(ctx, u); cmd.handled {
		_ = telegram.SendChunked(ctx, h.bot, u.ChatID, cmd.reply)
		return
	}
	h.handleMessage(ctx, u)
}
```

And update `cmd/otto/main.go` to set `startedAt`:

```go
h := &handler{
	bot:       bot,
	allow:     allow,
	session:   session,
	runner:    runner,
	startedAt: time.Now(),
}
```

(Add `"time"` to main.go imports.)

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./cmd/otto/...`
Expected: all tests including new command tests pass.

- [ ] **Step 5: Build**

Run: `go build -o ./otto ./cmd/otto`
Expected: clean build.

- [ ] **Step 6: Commit**

```bash
git add cmd/otto/commands.go cmd/otto/commands_test.go cmd/otto/handler.go cmd/otto/main.go
git commit -m "feat(cmd/otto): add /new /whoami /restart /status commands"
```

---

## Task 14: `cmd/otto` — image attachment forwarding

**Files:**
- Modify: `cmd/otto/handler.go`
- Modify: `cmd/otto/handler_test.go`

- [ ] **Step 1: Add the failing test**

Append to `cmd/otto/handler_test.go`:

```go
type fakeBotWithDownload struct {
	fakeBot
	files map[string][]byte
	cts   map[string]string
}

func (f *fakeBotWithDownload) DownloadFile(ctx context.Context, fileID string) ([]byte, string, error) {
	return f.files[fileID], f.cts[fileID], nil
}

func TestHandlerForwardsPhotoToClaude(t *testing.T) {
	bot := &fakeBotWithDownload{
		fakeBot: fakeBot{
			updates: [][]telegram.Update{{{UpdateID: 1, ChatID: 100, UserID: 99, Text: "describe this", PhotoIDs: []string{"PHOTO-1"}}}},
		},
		files: map[string][]byte{"PHOTO-1": []byte("\x89PNG fake")},
		cts:   map[string]string{"PHOTO-1": "image/png"},
	}
	runner := &fakeRunner{respond: "an image"}
	h := newTestHandler(t, bot, runner)

	runForBriefWindow(t, h)

	if len(runner.called) != 1 {
		t.Fatalf("runner.called = %d", len(runner.called))
	}
	if len(runner.called[0].ImagePaths) != 1 {
		t.Fatalf("image paths = %v", runner.called[0].ImagePaths)
	}
	if !strings.HasSuffix(runner.called[0].ImagePaths[0], ".png") {
		t.Errorf("image path = %q (want .png)", runner.called[0].ImagePaths[0])
	}
	// File should be cleaned up after the call.
	if _, err := os.Stat(runner.called[0].ImagePaths[0]); !os.IsNotExist(err) {
		t.Errorf("temp file not cleaned up: %v", err)
	}
}
```

(Add `"os"` to `handler_test.go` imports — `"strings"` is already there from Task 12.)

- [ ] **Step 2: Run test — expect failure**

Run: `go test ./cmd/otto/... -run TestHandlerForwardsPhoto`
Expected: image paths empty.

- [ ] **Step 3: Modify `cmd/otto/handler.go` to download photos**

Update `handleMessage` to download photos and pass paths:

```go
func (h *handler) handleMessage(ctx context.Context, u telegram.Update) {
	callCtx, cancel := context.WithTimeout(ctx, claudeCallTimeout)
	defer cancel()

	tmpDir, err := os.MkdirTemp("", "otto-photos-")
	if err != nil {
		_ = telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ tempdir: %v", err))
		return
	}
	defer os.RemoveAll(tmpDir)

	var imagePaths []string
	for _, pid := range u.PhotoIDs {
		path, err := telegram.DownloadPhotoToTemp(callCtx, h.bot, pid, tmpDir)
		if err != nil {
			_ = telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ photo download: %v", err))
			return
		}
		imagePaths = append(imagePaths, path)
	}

	events := make(chan claude.Event, 64)
	doneParsing := make(chan struct{})
	var assistantText strings.Builder

	go func() {
		defer close(doneParsing)
		for ev := range events {
			if t, ok := ev.(claude.AssistantTextEvent); ok {
				assistantText.WriteString(t.Text)
			}
		}
	}()

	err = h.runner.Run(callCtx, claude.RunArgs{
		Prompt:     u.Text,
		SessionID:  h.session.ID(),
		ImagePaths: imagePaths,
		Events:     events,
	})
	close(events)
	<-doneParsing

	if err != nil {
		_ = telegram.SendChunked(ctx, h.bot, u.ChatID, fmt.Sprintf("⚠️ Claude error: %s", err))
		return
	}
	out := strings.TrimSpace(assistantText.String())
	if out == "" {
		out = "(no response)"
	}
	if err := telegram.SendChunked(ctx, h.bot, u.ChatID, out); err != nil {
		log.Printf("send error: %v", err)
	}
}
```

(Add `"os"` to handler.go imports.)

- [ ] **Step 4: Run tests — expect pass**

Run: `go test ./cmd/otto/...`
Expected: all pass including `TestHandlerForwardsPhotoToClaude`.

- [ ] **Step 5: Build and full sweep**

Run: `go vet ./... && go test ./... && go build -o ./otto ./cmd/otto`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/otto/handler.go cmd/otto/handler_test.go
git commit -m "feat(cmd/otto): forward photo attachments to Claude Code"
```

---

## Task 15: Manual smoke test (no code change — verification only)

**Files:** none (operational verification).

This task verifies the binary works end-to-end against a real Telegram bot. It requires a real Telegram bot token, your Telegram user ID, and an Anthropic API key. If you don't have these handy yet, mark this task complete and revisit during Plan 3.

- [ ] **Step 1: Prepare a minimal config**

Create `~/.config/otto/config.toml` with real values, perms `0600`:

```toml
telegram_bot_token = "<from @BotFather>"
telegram_allowed_user_id = <your numeric user ID from @userinfobot>
anthropic_api_key = "<your Anthropic API key>"
notion_api_key = "unused-for-this-test"
claude_binary_path = "/usr/bin/claude"  # adjust if installed elsewhere
mcp_config_path = "/tmp/mcp-empty.json"
session_id_path = "/tmp/otto-test-session"
```

Create `/tmp/mcp-empty.json`:

```json
{ "mcpServers": {} }
```

- [ ] **Step 2: Run otto in foreground**

Run: `./otto`
Expected log: `otto: starting; session=<uuid> allowed_user=<your id>`

- [ ] **Step 3: Send "hi" from your Telegram**

Open the bot in Telegram, send `hi`. Expected: bot responds with text from Claude (any greeting). Logs show no errors.

- [ ] **Step 4: Test session persistence**

Send `My name is X`, wait for response. Send `What is my name?` — Claude should answer "X" because `--resume` keeps context.

- [ ] **Step 5: Test `/new`**

Send `/new`, then `What is my name?` — Claude should NOT remember (new session).

- [ ] **Step 6: Test image**

Send any photo with caption "describe this". Claude should respond with a description. (If the `@<path>` syntax is wrong for the installed Claude Code version, this is the moment to discover it — adjust `internal/claude/runner.go` `Run` accordingly and re-run tests.)

- [ ] **Step 7: Stop with Ctrl-C**

Logs: `otto: received interrupt, shutting down` then `otto: stopped`. Exit cleanly.

- [ ] **Step 8: Commit smoke-test notes**

If you found and fixed a bug (e.g., image syntax), commit the fix:

```bash
git add internal/claude/runner.go
git commit -m "fix(claude): correct image attachment CLI syntax"
```

If everything worked first try, no commit needed — just mark this task done.

---

## Done criteria for Plan 1

- `go vet ./... && go test ./... && go build -o ./otto ./cmd/otto` all green.
- The binary, given a valid `config.toml` and a minimal `mcp.json`, long-polls Telegram, validates the sender, forwards text + images to Claude Code, streams responses, supports `/new` `/whoami` `/restart` `/status`, and shuts down cleanly on SIGINT/SIGTERM.
- All four bot commands tested manually.
- Persistent session memory verified via "remember my name" → restart → recall.
- No Google MCP, no setup.sh, no systemd unit yet — those are Plans 2 and 3.
