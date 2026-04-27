// Package permissions tracks pending tool-permission decisions surfaced to
// the user via Telegram inline-keyboard buttons, and writes "allow always"
// rules into Claude Code's ~/.claude/settings.json.
package permissions

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Pending tracks button-click decisions awaiting a tap. Entries are short-
// lived: GC drops anything older than maxAge so a forgotten message
// doesn't pin memory. Capped to prevent unbounded growth on a chatty bot.
type Pending struct {
	mu      sync.Mutex
	entries map[string]Entry
	cap     int
}

// Entry is what we remember about a pending denial. ChatID/Prompt/SessionID
// give the caller enough context to auto-replay the original message after
// the user taps Allow — no manual re-send required.
type Entry struct {
	ToolName  string
	Pattern   string // settings.json-bound permission pattern
	ChatID    int64
	Prompt    string
	SessionID string
	CreatedAt time.Time
}

// New returns a Pending limited to capItems concurrent entries. When at
// capacity, oldest entries are evicted on Add.
func New(capItems int) *Pending {
	if capItems <= 0 {
		capItems = 32
	}
	return &Pending{entries: make(map[string]Entry, capItems), cap: capItems}
}

// Add stores an entry and returns a short opaque ID safe to embed in
// Telegram callback_data (≤64 bytes total per Telegram's API). CreatedAt
// on the input is ignored — Add stamps it with time.Now.
func (p *Pending) Add(e Entry) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) >= p.cap {
		p.evictOldestLocked()
	}
	id := newID()
	e.CreatedAt = time.Now()
	p.entries[id] = e
	return id
}

// Take removes and returns the entry for id. Returns ok=false if id is
// unknown (already taken, or expired and GC'd).
func (p *Pending) Take(id string) (Entry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.entries[id]
	if ok {
		delete(p.entries, id)
	}
	return e, ok
}

// GC drops entries older than maxAge. Safe to call periodically; cheap on
// small maps.
func (p *Pending) GC(maxAge time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for id, e := range p.entries {
		if e.CreatedAt.Before(cutoff) {
			delete(p.entries, id)
		}
	}
}

func (p *Pending) evictOldestLocked() {
	var oldestID string
	var oldestAt time.Time
	for id, e := range p.entries {
		if oldestID == "" || e.CreatedAt.Before(oldestAt) {
			oldestID, oldestAt = id, e.CreatedAt
		}
	}
	if oldestID != "" {
		delete(p.entries, oldestID)
	}
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// PatternFor turns a tool name into a permission pattern suitable for
// ~/.claude/settings.json's permissions.allow array. MCP tools become a
// wildcard over the whole server family ("mcp__gmail-personal__*"), so a
// single approval covers all gmail-personal calls; built-in tool names are
// returned verbatim.
func PatternFor(toolName string) string {
	if strings.HasPrefix(toolName, "mcp__") {
		rest := strings.TrimPrefix(toolName, "mcp__")
		if i := strings.LastIndex(rest, "__"); i > 0 {
			return "mcp__" + rest[:i] + "__*"
		}
	}
	return toolName
}

// AllowTool atomically adds pattern to the permissions.allow array in
// Claude Code's settings.json, preserving any other keys. Creates the
// file (and its parent dir) if missing. No-op if pattern is already
// present.
func AllowTool(settingsPath, pattern string) error {
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0700); err != nil {
		return fmt.Errorf("permissions: ensure dir: %w", err)
	}

	var doc map[string]any
	if data, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("permissions: parse %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("permissions: read %s: %w", settingsPath, err)
	}
	if doc == nil {
		doc = map[string]any{}
	}

	perms, _ := doc["permissions"].(map[string]any)
	if perms == nil {
		perms = map[string]any{}
		doc["permissions"] = perms
	}
	allow, _ := perms["allow"].([]any)
	for _, existing := range allow {
		if s, ok := existing.(string); ok && s == pattern {
			return nil // already present
		}
	}
	perms["allow"] = append(allow, pattern)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("permissions: marshal: %w", err)
	}
	tmp := settingsPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return fmt.Errorf("permissions: write tmp: %w", err)
	}
	if err := os.Rename(tmp, settingsPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("permissions: rename: %w", err)
	}
	return nil
}
