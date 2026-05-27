# Otto Session Rotation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound Otto's per-turn token cost by auto-rotating his Claude session when it grows large — but only during idle gaps so it never interrupts a live conversation. When the tracked session input-token count crosses a soft threshold (default 50% of the model's context) AND the user has been quiet for an idle window (default 15 min), or unconditionally at a hard threshold (default 85%), Otto clears the session; the next message starts fresh, seeded by the always-injected memory core.

**Architecture:** The stream-json parser captures `usage.input_tokens` from the result event (for a `--resume` session this approximates the whole transcript size). The handler records the latest value plus the time of the last user message. A long-lived rotator goroutine (mirroring the existing updater/watchdog pattern) ticks once a minute and, when Otto is idle and a pure `shouldRotate` decision says so, claims the Otto slot and calls `Session.Clear`. Continuity across a rotation comes from the persistent memory core (Plans 1–4c) and `session_search` — this v1 deliberately omits an LLM-based flush/handoff to keep the hot-path change minimal and deterministic (noted under Non-goals).

**Tech Stack:** Go 1.26, existing `internal/claude` (parser/event), `internal/config`, `cmd/otto` (handler/main, build tag `//go:build unix`).

## Non-goals (v1)
- LLM "flush" pass at rotation (distill session → memory_add). Inline `memory_add` during the conversation already persists facts; the always-injected core carries them across the clear. A flush is a later enhancement.
- An LLM "handoff note" summarizing the open thread. Rotation is idle-gated (≥15 min quiet), so the thread is almost always already concluded; `session_search` recovers older context on demand.

---

## Context on existing code

- `internal/claude/parser.go`: `rawMessage` struct unmarshals stream-json; the `"result"` case builds `ResultEvent{Subtype, Error, PermissionDenials}`.
- `internal/claude/event.go`: defines `ResultEvent` (fields `Subtype string`, `Error string`, `PermissionDenials []PermissionDenial`). (Read it to confirm before editing.)
- `cmd/otto/handler.go`: `ottoState` struct (mutex-guarded: `busy`, `currentPrompt`, `cancel`, `lastEvent`, `suppressError`, `lastSnippet`) with methods `tryAcquire`, `release`, `markEvent`, `Snapshot`, etc. `runAndReply` captures `lastResult claude.ResultEvent`. `dispatch(ctx, u)` is the per-message entry (after allowlist check). `handler` has fields incl. `session *claude.Session`, `otto *ottoState`, `startedAt`.
- `cmd/otto/watchdog.go`: `runWatchdog` shows the ticker+select pattern. `cmd/otto/updater.go`'s `Run` shows a long-lived goroutine started from main with `go h.updater.Run(ctx)`.
- `cmd/otto/main.go`: builds the handler, starts `go h.updater.Run(ctx)`, then `h.runPollingLoop(ctx)`.
- `cmd/otto/commands.go`: `/new` calls `h.session.Clear()`.
- `internal/config`: `Load` applies defaults after `validate()`.

## File Structure

- `internal/claude/event.go` (modify) — `ResultEvent.InputTokens int`.
- `internal/claude/parser.go` (modify) — parse `usage.input_tokens`.
- `internal/claude/parser_test.go` (modify) — assert token capture.
- `internal/config/config.go` (modify) — rotation threshold fields + defaults.
- `internal/config/config_test.go` (modify) — defaults test.
- `cmd/otto/rotate.go` (create) — `rotateConfig` struct + pure `shouldRotate` + `runRotator`.
- `cmd/otto/rotate_test.go` (create) — `shouldRotate` table tests.
- `cmd/otto/handler.go` (modify) — `ottoState` gains `lastInputTokens` + `lastUserMsg`; setters; `dispatch` marks user activity; `runAndReply` records tokens; `handler` gains `rotate rotateConfig`.
- `cmd/otto/commands.go` (modify) — `/new` also resets token count.
- `cmd/otto/main.go` (modify) — populate `h.rotate` from config; `go h.runRotator(ctx)`.

---

## Task 1: parser captures input_tokens

**Files:**
- Modify: `internal/claude/event.go`
- Modify: `internal/claude/parser.go`
- Modify: `internal/claude/parser_test.go`

- [ ] **Step 1: Write the failing test**

First inspect `internal/claude/parser_test.go` to match its existing style (how it feeds lines to `ParseStream` and collects events). Then append a test. If the file has a helper that runs `ParseStream` over a string and returns `[]Event`, reuse it; otherwise this self-contained test works:
```go
func TestParseStreamCapturesInputTokens(t *testing.T) {
	line := `{"type":"result","subtype":"success","usage":{"input_tokens":4242,"output_tokens":17},"session_id":"s1"}` + "\n"
	events := make(chan Event, 8)
	go func() {
		_ = ParseStream(context.Background(), strings.NewReader(line), events)
		close(events)
	}()
	var got ResultEvent
	var found bool
	for ev := range events {
		if r, ok := ev.(ResultEvent); ok {
			got = r
			found = true
		}
	}
	if !found {
		t.Fatal("no ResultEvent emitted")
	}
	if got.InputTokens != 4242 {
		t.Fatalf("InputTokens = %d, want 4242", got.InputTokens)
	}
}
```
Ensure `parser_test.go` imports `context` and `strings` (add if missing).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/claude/ -run TestParseStreamCapturesInputTokens -v`
Expected: FAIL — `got.InputTokens undefined` (compile error).

- [ ] **Step 3: Write minimal implementation**

(a) In `internal/claude/event.go`, add a field to `ResultEvent`:
```go
	InputTokens int // usage.input_tokens from the result event; 0 if absent
```
(Add it to the existing `ResultEvent` struct alongside `Subtype`/`Error`/`PermissionDenials`.)

(b) In `internal/claude/parser.go`, add a `Usage` field to `rawMessage` (anywhere in the struct):
```go
	Usage struct {
		InputTokens int `json:"input_tokens"`
	} `json:"usage"`
```
(c) In the `"result"` case of `ParseStream`, include the token count when building the event. Change:
```go
		ev := ResultEvent{Subtype: raw.Subtype, Error: raw.Error}
```
to:
```go
		ev := ResultEvent{Subtype: raw.Subtype, Error: raw.Error, InputTokens: raw.Usage.InputTokens}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/claude/ -v`
Expected: PASS (new test + all existing parser tests — existing result events without `usage` get `InputTokens: 0`, unchanged behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/claude/event.go internal/claude/parser.go internal/claude/parser_test.go
git commit -m "feat(claude): capture usage.input_tokens from result event"
```

---

## Task 2: rotation config thresholds

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Write the failing test**

Append to `internal/config/config_test.go`:
```go
func TestLoadDerivesRotationDefaults(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	mcp := filepath.Join(dir, "mcp.json")
	for _, p := range []string{bin, mcp} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	body := "telegram_bot_token = \"t\"\n" +
		"telegram_allowed_user_id = 5\n" +
		"claude_binary_path = \"" + bin + "\"\n" +
		"mcp_config_path = \"" + mcp + "\"\n" +
		"session_id_path = \"" + dir + "/session_id\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelContextTokens != 200000 {
		t.Errorf("ModelContextTokens default = %d", cfg.ModelContextTokens)
	}
	if cfg.RotateSoftPct != 0.50 {
		t.Errorf("RotateSoftPct default = %v", cfg.RotateSoftPct)
	}
	if cfg.RotateHardPct != 0.85 {
		t.Errorf("RotateHardPct default = %v", cfg.RotateHardPct)
	}
	if cfg.RotateIdleMinutes != 15 {
		t.Errorf("RotateIdleMinutes default = %d", cfg.RotateIdleMinutes)
	}
}

func TestLoadHonorsExplicitRotation(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	mcp := filepath.Join(dir, "mcp.json")
	for _, p := range []string{bin, mcp} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	body := "telegram_bot_token = \"t\"\n" +
		"telegram_allowed_user_id = 5\n" +
		"claude_binary_path = \"" + bin + "\"\n" +
		"mcp_config_path = \"" + mcp + "\"\n" +
		"session_id_path = \"" + dir + "/session_id\"\n" +
		"model_context_tokens = 100000\n" +
		"rotate_soft_pct = 0.4\n" +
		"rotate_hard_pct = 0.9\n" +
		"rotate_idle_minutes = 5\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelContextTokens != 100000 || cfg.RotateSoftPct != 0.4 || cfg.RotateHardPct != 0.9 || cfg.RotateIdleMinutes != 5 {
		t.Errorf("explicit rotation config not honored: %+v", cfg)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestLoad.*Rotation -v`
Expected: FAIL — fields undefined (compile error).

- [ ] **Step 3: Write minimal implementation**

In `internal/config/config.go`, add fields to `Config` (after `EmbedModels`):
```go
	// ModelContextTokens is Otto's model context window, used as the denominator
	// for rotation thresholds. Default 200000.
	ModelContextTokens int `toml:"model_context_tokens"`
	// RotateSoftPct: at this fraction of context, the session is eligible to
	// rotate once the user goes idle. Default 0.50.
	RotateSoftPct float64 `toml:"rotate_soft_pct"`
	// RotateHardPct: at this fraction, rotate at the next idle tick regardless
	// of how recently the user spoke (safety cap). Default 0.85.
	RotateHardPct float64 `toml:"rotate_hard_pct"`
	// RotateIdleMinutes: minutes of user silence required before a soft-eligible
	// session rotates. Default 15.
	RotateIdleMinutes int `toml:"rotate_idle_minutes"`
```
In `Load`, after the embed defaults block, add:
```go
	if cfg.ModelContextTokens <= 0 {
		cfg.ModelContextTokens = 200000
	}
	if cfg.RotateSoftPct <= 0 {
		cfg.RotateSoftPct = 0.50
	}
	if cfg.RotateHardPct <= 0 {
		cfg.RotateHardPct = 0.85
	}
	if cfg.RotateIdleMinutes <= 0 {
		cfg.RotateIdleMinutes = 15
	}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): session-rotation thresholds with defaults"
```

---

## Task 3: pure rotation decision

**Files:**
- Create: `cmd/otto/rotate.go`
- Test: `cmd/otto/rotate_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/otto/rotate_test.go`:
```go
//go:build unix

package main

import (
	"testing"
	"time"
)

func testRotateConfig() rotateConfig {
	return rotateConfig{
		ctxTokens:  200000,
		soft:       0.50,
		hard:       0.85,
		idleWindow: 15 * time.Minute,
	}
}

func TestShouldRotateBelowSoftNeverRotates(t *testing.T) {
	c := testRotateConfig()
	// 40% of context, very idle — still below soft, no rotation.
	if shouldRotate(80000, time.Hour, c) {
		t.Error("below soft threshold should never rotate")
	}
}

func TestShouldRotateSoftWaitsForIdle(t *testing.T) {
	c := testRotateConfig()
	// 60% of context but user active 1 min ago — wait.
	if shouldRotate(120000, 1*time.Minute, c) {
		t.Error("soft-eligible but not idle should not rotate")
	}
	// 60% and idle 20 min — rotate.
	if !shouldRotate(120000, 20*time.Minute, c) {
		t.Error("soft-eligible and idle should rotate")
	}
}

func TestShouldRotateHardIgnoresIdle(t *testing.T) {
	c := testRotateConfig()
	// 90% of context, user active 10s ago — hard cap rotates anyway.
	if !shouldRotate(180000, 10*time.Second, c) {
		t.Error("hard threshold should rotate regardless of idle")
	}
}

func TestShouldRotateZeroTokens(t *testing.T) {
	c := testRotateConfig()
	if shouldRotate(0, time.Hour, c) {
		t.Error("zero tokens (no session activity) should not rotate")
	}
}

func TestShouldRotateZeroCtxIsSafe(t *testing.T) {
	c := testRotateConfig()
	c.ctxTokens = 0
	if shouldRotate(100000, time.Hour, c) {
		t.Error("zero ctxTokens must not divide-by-zero into a rotation")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto/ -run TestShouldRotate -v`
Expected: FAIL — `undefined: rotateConfig` / `undefined: shouldRotate`.

- [ ] **Step 3: Write minimal implementation**

Create `cmd/otto/rotate.go`:
```go
//go:build unix

package main

import (
	"context"
	"log"
	"time"
)

// rotateCheckInterval is how often the rotator evaluates whether to rotate.
const rotateCheckInterval = 1 * time.Minute

// rotateConfig holds the rotation thresholds, resolved from config at startup.
type rotateConfig struct {
	ctxTokens  int           // model context window (denominator)
	soft       float64       // fraction → eligible once idle
	hard       float64       // fraction → rotate regardless of idle
	idleWindow time.Duration // required user silence for a soft rotation
}

// shouldRotate decides whether the current session should be cleared.
// tokens is the latest observed session input-token count; idle is how long
// since the last user message. Returns false for a zero/invalid context size
// (no divide-by-zero) and for an empty/idle session with no tokens.
func shouldRotate(tokens int, idle time.Duration, c rotateConfig) bool {
	if c.ctxTokens <= 0 || tokens <= 0 {
		return false
	}
	frac := float64(tokens) / float64(c.ctxTokens)
	if frac >= c.hard {
		return true
	}
	if frac >= c.soft && idle >= c.idleWindow {
		return true
	}
	return false
}

// runRotator is a long-lived goroutine (started from main) that periodically
// clears Otto's session once it has grown past a threshold and the user is
// idle, bounding per-turn token cost. It claims the Otto slot before clearing
// so it can never race a live turn; if Otto is busy it simply waits for the
// next tick. Exits when ctx is cancelled.
func (h *handler) runRotator(ctx context.Context) {
	if h.rotate.ctxTokens <= 0 {
		log.Printf("rotator: disabled (ctxTokens<=0)")
		return
	}
	ticker := time.NewTicker(rotateCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if h.session.ID() == "" {
				continue // no active session to rotate
			}
			tokens, idle := h.otto.rotationSnapshot()
			if !shouldRotate(tokens, idle, h.rotate) {
				continue
			}
			// Claim the slot so a new turn can't start mid-rotation. If Otto
			// is busy, skip and retry next tick.
			if !h.otto.tryAcquire("(session rotation)") {
				continue
			}
			err := h.session.Clear()
			h.otto.resetInputTokens()
			h.otto.release()
			if err != nil {
				log.Printf("rotator: clear session: %v", err)
			} else {
				log.Printf("rotator: rotated session (tokens=%d idle=%s) — next message starts fresh", tokens, idle.Round(time.Second))
			}
		}
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

The `runRotator` method references `h.rotate`, `h.otto.rotationSnapshot()`, and `h.otto.resetInputTokens()` which are added in Task 4 — so the package will not compile until Task 4 lands. Run only the pure-function test now by temporarily expecting a compile dependency: implement Task 4 before running the full package. To verify the pure logic in isolation first, you may comment out `runRotator` while running:

Run: `go test ./cmd/otto/ -run TestShouldRotate -v` (after Task 4, or with runRotator temporarily stubbed)
Expected: PASS (all five subtests).

NOTE: To keep this task's commit compiling on its own, include in this commit ONLY the `rotateCheckInterval`, `rotateConfig`, and `shouldRotate` definitions (not `runRotator`). Add `runRotator` in Task 4 alongside the `ottoState`/`handler` fields it depends on. So for Step 3 of THIS task, create `cmd/otto/rotate.go` with just:
```go
//go:build unix

package main

import "time"

// rotateCheckInterval is how often the rotator evaluates whether to rotate.
const rotateCheckInterval = 1 * time.Minute

// rotateConfig holds the rotation thresholds, resolved from config at startup.
type rotateConfig struct {
	ctxTokens  int
	soft       float64
	hard       float64
	idleWindow time.Duration
}

// shouldRotate decides whether the current session should be cleared. tokens is
// the latest observed session input-token count; idle is how long since the
// last user message. Returns false for a zero/invalid context size (no
// divide-by-zero) and for a session with no observed tokens.
func shouldRotate(tokens int, idle time.Duration, c rotateConfig) bool {
	if c.ctxTokens <= 0 || tokens <= 0 {
		return false
	}
	frac := float64(tokens) / float64(c.ctxTokens)
	if frac >= c.hard {
		return true
	}
	if frac >= c.soft && idle >= c.idleWindow {
		return true
	}
	return false
}
```
(`runRotator` is added in Task 4.)

- [ ] **Step 5: Commit**

```bash
git add cmd/otto/rotate.go cmd/otto/rotate_test.go
git commit -m "feat(otto): pure shouldRotate decision + rotateConfig"
```

---

## Task 4: track tokens + activity, run the rotator

**Files:**
- Modify: `cmd/otto/handler.go`
- Modify: `cmd/otto/rotate.go` (add `runRotator`)
- Modify: `cmd/otto/commands.go`
- Modify: `cmd/otto/main.go`
- Modify: `cmd/otto/handler_test.go`

- [ ] **Step 1: Write the failing test**

Append to `cmd/otto/handler_test.go`:
```go
func TestOttoStateTokenAndActivityTracking(t *testing.T) {
	s := newOttoState()
	s.setInputTokens(1234)
	s.markUserMessage()
	tokens, idle := s.rotationSnapshot()
	if tokens != 1234 {
		t.Errorf("tokens = %d, want 1234", tokens)
	}
	if idle > time.Second {
		t.Errorf("idle = %s, want ~0 right after markUserMessage", idle)
	}
	s.resetInputTokens()
	tokens, _ = s.rotationSnapshot()
	if tokens != 0 {
		t.Errorf("after reset tokens = %d, want 0", tokens)
	}
}

func TestRunRotatorClearsLargeIdleSession(t *testing.T) {
	bot := &fakeBot{}
	runner := &fakeRunner{}
	h := newTestHandler(t, bot, runner)
	h.rotate = rotateConfig{ctxTokens: 1000, soft: 0.5, hard: 0.85, idleWindow: 0}
	// Give the session a real ID so rotation has something to clear.
	if err := h.session.Set("sess-xyz"); err != nil {
		t.Fatal(err)
	}
	h.otto.setInputTokens(900) // 90% → over hard
	// idleWindow 0 means any idle qualifies.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runRotator(ctx)

	deadline := time.After(3 * time.Second)
	for {
		if h.session.ID() == "" {
			break // rotated
		}
		select {
		case <-deadline:
			t.Fatal("session was not rotated within 3s")
		case <-time.After(50 * time.Millisecond):
		}
	}
}
```
(handler_test.go already imports `context`, `time`, `testing`.)

NOTE: `rotateCheckInterval` is 1 minute, too slow for the test. To make the rotator testable, the test above relies on it; instead, lower the loop's first check by making `runRotator` evaluate once immediately before entering the ticker loop. Implement that (see Step 3c). With an immediate first evaluation, the test passes in milliseconds.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/otto/ -run 'TestOttoStateToken|TestRunRotator' -v`
Expected: FAIL — `s.setInputTokens` / `rotationSnapshot` / `resetInputTokens` / `runRotator` / `h.rotate` undefined.

- [ ] **Step 3: Write minimal implementation**

(a) In `cmd/otto/handler.go`, add fields to `ottoState` (under the mutex, after `lastSnippet`):
```go
	lastInputTokens int       // usage.input_tokens of the most recent Otto turn
	lastUserMsg     time.Time // time of the most recent user message (for idle calc)
```
And add methods (near the other `ottoState` methods):
```go
// setInputTokens records the session's latest observed input-token count.
func (s *ottoState) setInputTokens(n int) {
	s.mu.Lock()
	s.lastInputTokens = n
	s.mu.Unlock()
}

// resetInputTokens zeroes the token count (after a session clear/rotation).
func (s *ottoState) resetInputTokens() {
	s.mu.Lock()
	s.lastInputTokens = 0
	s.mu.Unlock()
}

// markUserMessage records that the user just sent a message (resets idle).
func (s *ottoState) markUserMessage() {
	s.mu.Lock()
	s.lastUserMsg = time.Now()
	s.mu.Unlock()
}

// rotationSnapshot returns the latest token count and how long since the last
// user message (idle). If no user message has been seen, idle is 0.
func (s *ottoState) rotationSnapshot() (tokens int, idle time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens = s.lastInputTokens
	if !s.lastUserMsg.IsZero() {
		idle = time.Since(s.lastUserMsg)
	}
	return tokens, idle
}
```

(b) Add a `rotate rotateConfig` field to the `handler` struct (after `pets` or near the other config-derived fields).

(c) In `cmd/otto/rotate.go`, add the `runRotator` method (it needs `context` and `log` imports — update the import block):
```go
// runRotator is a long-lived goroutine (started from main) that periodically
// clears Otto's session once it has grown past a threshold and the user is
// idle, bounding per-turn token cost. It claims the Otto slot before clearing
// so it can never race a live turn; if Otto is busy it waits for the next
// tick. Exits when ctx is cancelled.
func (h *handler) runRotator(ctx context.Context) {
	if h.rotate.ctxTokens <= 0 {
		log.Printf("rotator: disabled (ctxTokens<=0)")
		return
	}
	ticker := time.NewTicker(rotateCheckInterval)
	defer ticker.Stop()
	// Evaluate immediately, then on each tick. The immediate pass keeps the
	// rotator responsive (and makes it testable without waiting a full tick).
	for {
		h.maybeRotate()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// maybeRotate performs one rotation evaluation: if the session is non-empty,
// over threshold, and Otto is free, clear it.
func (h *handler) maybeRotate() {
	if h.session.ID() == "" {
		return
	}
	tokens, idle := h.otto.rotationSnapshot()
	if !shouldRotate(tokens, idle, h.rotate) {
		return
	}
	if !h.otto.tryAcquire("(session rotation)") {
		return // Otto busy; retry next tick
	}
	err := h.session.Clear()
	h.otto.resetInputTokens()
	h.otto.release()
	if err != nil {
		log.Printf("rotator: clear session: %v", err)
		return
	}
	log.Printf("rotator: rotated session (tokens=%d idle=%s) — next message starts fresh", tokens, idle.Round(time.Second))
}
```
Update `cmd/otto/rotate.go`'s import block to `import ("context"; "log"; "time")`.

(d) In `cmd/otto/handler.go` `dispatch`, right after the allowlist check passes (after `if !h.allow.Allows(u.UserID) { ... return }`), record activity:
```go
	h.otto.markUserMessage()
```

(e) In `cmd/otto/handler.go` `runAndReply`, after `lastResult` is finalized (right where the success path records things, e.g. just before or after sending), record the token count:
```go
	h.otto.setInputTokens(lastResult.InputTokens)
```
Place it after the `err := h.runner.Run(...)` block and after `lastResult` is known (e.g. immediately after the `<-doneParsing`/result handling, unconditionally — a non-success result still carries a usage count, and 0 is harmless).

(f) In `cmd/otto/commands.go`, the `/new` handler calls `h.session.Clear()`. After a successful clear, also reset the token count so the fresh session starts the rotator's counter at 0:
```go
		h.otto.resetInputTokens()
```
(Add immediately after the successful `h.session.Clear()` in the `/new` case.)

(g) In `cmd/otto/main.go`, populate `h.rotate` when building the handler and start the rotator. After the `h := &handler{...}` construction (or as a field in the literal), set:
```go
	h.rotate = rotateConfig{
		ctxTokens:  cfg.ModelContextTokens,
		soft:       cfg.RotateSoftPct,
		hard:       cfg.RotateHardPct,
		idleWindow: time.Duration(cfg.RotateIdleMinutes) * time.Minute,
	}
```
And alongside `go h.updater.Run(ctx)`, add:
```go
	go h.runRotator(ctx)
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/otto/ -v`
Expected: PASS — new tests + all existing (existing tests leave `h.rotate` zero-valued → `runRotator` would early-return as disabled; they don't start it, so unaffected).

- [ ] **Step 5: Race check**

Run: `go test -race ./cmd/otto/`
Expected: PASS, no races (the rotator reads `ottoState` under the same mutex as everything else).

- [ ] **Step 6: Commit**

```bash
git add cmd/otto/handler.go cmd/otto/rotate.go cmd/otto/commands.go cmd/otto/main.go cmd/otto/handler_test.go
git commit -m "feat(otto): track session tokens + idle, run idle-gated session rotator"
```

---

## Task 5: Final verification

- [ ] **Step 1: Vet + format**

Run:
```bash
go vet ./...
gofmt -l cmd/otto/ internal/claude/ internal/config/
```
Expected: vet 0; gofmt nothing.

- [ ] **Step 2: Build + full test + race**

Run:
```bash
go build ./...
go test ./...
go test -race ./cmd/otto/ ./internal/claude/ ./internal/config/
```
Expected: all pass.

- [ ] **Step 3: Binaries build + wiring sanity**

Run:
```bash
go build -o /tmp/otto ./cmd/otto && echo "otto OK" && rm -f /tmp/otto
grep -n "runRotator\|h.rotate" cmd/otto/main.go
grep -n "setInputTokens\|markUserMessage" cmd/otto/handler.go
```
Expected: `otto OK`; `runRotator`+`h.rotate` in main.go; `setInputTokens`+`markUserMessage` in handler.go.

---

## Self-Review notes

- **Spec coverage (this slice):** token capture from result event (Task 1); soft/hard/idle thresholds in config (Task 2); pure idle-gated decision incl. hard-cap-ignores-idle and divide-by-zero safety (Task 3); activity + token tracking, the rotator goroutine that claims the Otto slot before clearing, `/new` token reset, and main wiring (Task 4). Matches the spec's "rotate when (tokens≥soft AND idle≥window) OR tokens≥hard," idle-gated, slot-safe.
- **Deliberate v1 simplifications (Non-goals):** no LLM flush, no LLM handoff note — continuity is the always-injected memory core + `session_search`. Documented at top.
- **Type consistency:** `ResultEvent.InputTokens int`; config `ModelContextTokens int`, `RotateSoftPct/RotateHardPct float64`, `RotateIdleMinutes int`; `rotateConfig{ctxTokens int; soft, hard float64; idleWindow time.Duration}`; `shouldRotate(int, time.Duration, rotateConfig) bool`; `ottoState` methods `setInputTokens`/`resetInputTokens`/`markUserMessage`/`rotationSnapshot`; `handler.rotate rotateConfig`; `(*handler).runRotator(ctx)` + `maybeRotate()`.
- **Safety:** the rotator only ever clears while holding the Otto slot (`tryAcquire`), so it cannot run concurrently with a live turn; a busy Otto just defers rotation to the next tick. `ctxTokens<=0` disables the rotator and short-circuits `shouldRotate`. Existing `cmd/otto` tests are unaffected (zero-valued `h.rotate` → disabled; they never start `runRotator`).
- **Note for setup follow-up:** `setup.sh` may optionally `write_toml_field model_context_tokens/rotate_soft_pct/rotate_hard_pct/rotate_idle_minutes`, but defaults suffice, so it's not required.
```
