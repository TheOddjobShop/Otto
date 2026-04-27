package permissions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingAddTakeRoundTrip(t *testing.T) {
	p := New(8)
	id := p.Add(Entry{
		ToolName:  "mcp__gmail-personal__search_emails",
		Pattern:   "mcp__gmail-personal__*",
		ChatID:    100,
		Prompt:    "check my email",
		SessionID: "sess-abc",
	})
	if id == "" {
		t.Fatal("Add returned empty ID")
	}
	got, ok := p.Take(id)
	if !ok {
		t.Fatal("Take returned ok=false")
	}
	if got.ToolName != "mcp__gmail-personal__search_emails" {
		t.Errorf("ToolName = %q", got.ToolName)
	}
	if got.Pattern != "mcp__gmail-personal__*" {
		t.Errorf("Pattern = %q", got.Pattern)
	}
	if got.ChatID != 100 {
		t.Errorf("ChatID = %d", got.ChatID)
	}
	if got.Prompt != "check my email" {
		t.Errorf("Prompt = %q", got.Prompt)
	}
	if got.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q", got.SessionID)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be set by Add")
	}
	// Second Take of the same ID misses.
	if _, ok := p.Take(id); ok {
		t.Error("expected second Take to miss")
	}
}

func TestPendingTakeUnknownID(t *testing.T) {
	p := New(8)
	if _, ok := p.Take("nonexistent"); ok {
		t.Error("expected ok=false for unknown ID")
	}
}

func TestPendingEvictsOldestAtCap(t *testing.T) {
	p := New(2)
	id1 := p.Add(Entry{ToolName: "a", Pattern: "a*"})
	time.Sleep(2 * time.Millisecond)
	id2 := p.Add(Entry{ToolName: "b", Pattern: "b*"})
	time.Sleep(2 * time.Millisecond)
	p.Add(Entry{ToolName: "c", Pattern: "c*"}) // should evict id1
	if _, ok := p.Take(id1); ok {
		t.Error("oldest entry was not evicted")
	}
	if _, ok := p.Take(id2); !ok {
		t.Error("middle entry should still be present")
	}
}

func TestPendingGCDropsExpired(t *testing.T) {
	p := New(8)
	id := p.Add(Entry{ToolName: "a", Pattern: "a*"})
	time.Sleep(20 * time.Millisecond)
	p.GC(10 * time.Millisecond)
	if _, ok := p.Take(id); ok {
		t.Error("expired entry was not GC'd")
	}
}

func TestPatternForMCP(t *testing.T) {
	cases := map[string]string{
		"mcp__gmail-personal__search_emails": "mcp__gmail-personal__*",
		"mcp__gdrive__list":                  "mcp__gdrive__*",
		"mcp__notion__create_page":           "mcp__notion__*",
		"Bash":                               "Bash",
		"Read":                               "Read",
	}
	for in, want := range cases {
		if got := PatternFor(in); got != want {
			t.Errorf("PatternFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowToolWritesNewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "settings.json")
	if err := AllowTool(path, "mcp__gmail-personal__*"); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, path)
	allow := got["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 1 || allow[0].(string) != "mcp__gmail-personal__*" {
		t.Errorf("allow = %v", allow)
	}
}

func TestAllowToolPreservesExistingKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	initial := `{
  "model": "claude-sonnet-4-6",
  "permissions": {
    "deny": ["Bash(rm:*)"],
    "allow": ["Read"]
  },
  "extras": {"some": "thing"}
}`
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AllowTool(path, "mcp__gmail-personal__*"); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, path)
	if got["model"] != "claude-sonnet-4-6" {
		t.Errorf("model lost: %v", got["model"])
	}
	if got["extras"].(map[string]any)["some"] != "thing" {
		t.Errorf("extras lost: %v", got["extras"])
	}
	perms := got["permissions"].(map[string]any)
	deny := perms["deny"].([]any)
	if len(deny) != 1 || deny[0].(string) != "Bash(rm:*)" {
		t.Errorf("deny lost: %v", deny)
	}
	allow := perms["allow"].([]any)
	if len(allow) != 2 {
		t.Fatalf("allow len = %d, want 2", len(allow))
	}
}

func TestAllowToolDedupes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := AllowTool(path, "Read"); err != nil {
		t.Fatal(err)
	}
	if err := AllowTool(path, "Read"); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, path)
	allow := got["permissions"].(map[string]any)["allow"].([]any)
	if len(allow) != 1 {
		t.Errorf("allow should not have duplicate entries: %v", allow)
	}
}

func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
