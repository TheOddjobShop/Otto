//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildSystemPromptEmptyPathReturnsEmpty(t *testing.T) {
	got, err := buildSystemPrompt("", "/nonexistent/mcp.json")
	if err != nil {
		t.Fatalf("err = %v, want nil for empty path", err)
	}
	if got != "" {
		t.Errorf("expected empty result, got %d bytes", len(got))
	}
}

func TestBuildSystemPromptIncludesPersonaAndMCPFooter(t *testing.T) {
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "system_prompt.md")
	if err := os.WriteFile(personaPath, []byte("Be kind."), 0600); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"gmail":{},"notion":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}

	got, err := buildSystemPrompt(personaPath, mcpPath)
	if err != nil {
		t.Fatalf("buildSystemPrompt: %v", err)
	}
	if !strings.HasPrefix(got, "Be kind.") {
		t.Errorf("persona prefix missing; got: %q", got[:min(40, len(got))])
	}
	for _, want := range []string{"OPERATIONAL CONTEXT", "Telegram", "gmail", "notion"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected to find %q in built prompt", want)
		}
	}
}

func TestBuildSystemPromptToleratesMissingMCPConfig(t *testing.T) {
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "system_prompt.md")
	if err := os.WriteFile(personaPath, []byte("Be kind."), 0600); err != nil {
		t.Fatal(err)
	}
	// Point MCP path at a nonexistent file. Should not error out — we want
	// the bot to start with a degraded prompt rather than refuse to boot.
	got, err := buildSystemPrompt(personaPath, filepath.Join(dir, "nope.json"))
	if err != nil {
		t.Fatalf("expected nil err on missing mcp.json, got %v", err)
	}
	if !strings.Contains(got, "Be kind.") {
		t.Error("persona missing from degraded prompt")
	}
}

func TestDescribeServerRecognizes(t *testing.T) {
	cases := map[string]string{
		"gdrive":          "Google Drive",
		"google-calendar": "Google Calendar",
		"notion":          "Notion",
		"gmail":           "Gmail",
		"gmail-personal":  "personal account",
		"gmail-school":    "school account",
		"gmail-koro":      "koro account",
	}
	for name, wantSubstr := range cases {
		got := describeServer(name)
		if !strings.Contains(got, wantSubstr) {
			t.Errorf("describeServer(%q) = %q, want containing %q", name, got, wantSubstr)
		}
	}
}

func TestDescribeServerUnknownReturnsEmpty(t *testing.T) {
	if got := describeServer("some-future-mcp"); got != "" {
		t.Errorf("describeServer(unknown) = %q, want empty", got)
	}
}

func TestBuildSystemPromptListsMultipleGmailAccounts(t *testing.T) {
	dir := t.TempDir()
	personaPath := filepath.Join(dir, "system_prompt.md")
	if err := os.WriteFile(personaPath, []byte("Be kind."), 0600); err != nil {
		t.Fatal(err)
	}
	mcpPath := filepath.Join(dir, "mcp.json")
	mcp := `{"mcpServers":{"gmail-personal":{},"gmail-school":{},"notion":{}}}`
	if err := os.WriteFile(mcpPath, []byte(mcp), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := buildSystemPrompt(personaPath, mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gmail-personal", "personal account", "gmail-school", "school account"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in built prompt", want)
		}
	}
}

func TestWriteTotoMCPConfigOnlyIncludesOttoMemory(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	mcpPath := filepath.Join(dir, "mcp.json")
	full := `{"mcpServers":{` +
		`"gmail":{"command":"gmail-mcp","args":["--account","x"]},` +
		`"notion":{"command":"notion-mcp"},` +
		`"otto-memory":{"command":"otto-memory","args":["--memory-dir","/tmp/m","--state-db","/tmp/s.db"]}` +
		`}}`
	if err := os.WriteFile(mcpPath, []byte(full), 0600); err != nil {
		t.Fatal(err)
	}

	out, err := writeTotoMCPConfig(stateDir, mcpPath)
	if err != nil {
		t.Fatalf("writeTotoMCPConfig: %v", err)
	}
	if out == "" {
		t.Fatal("expected a non-empty path when otto-memory is present")
	}
	if filepath.Base(out) != "toto-mcp.json" {
		t.Errorf("expected toto-mcp.json basename, got %q", out)
	}

	// Permissions should be 0600 — this file is per-user, not shared.
	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat scoped config: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected 0600 perms, got %o", perm)
	}

	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("scoped config not valid json: %v", err)
	}
	if len(got.MCPServers) != 1 {
		t.Fatalf("expected exactly 1 server entry, got %d (%v)", len(got.MCPServers), got.MCPServers)
	}
	if _, ok := got.MCPServers["otto-memory"]; !ok {
		t.Errorf("otto-memory missing from scoped config: %s", string(body))
	}
	for _, forbidden := range []string{"gmail", "notion"} {
		if _, ok := got.MCPServers[forbidden]; ok {
			t.Errorf("scoped config must not include %q: %s", forbidden, string(body))
		}
	}
	// Defense in depth: also check the raw bytes don't even mention them.
	if strings.Contains(string(body), "gmail") || strings.Contains(string(body), "notion") {
		t.Errorf("scoped config leaks other server names: %s", string(body))
	}
}

func TestWriteTotoMCPConfigNoOttoMemoryReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	mcpPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpPath, []byte(`{"mcpServers":{"gmail":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := writeTotoMCPConfig(stateDir, mcpPath)
	if err != nil {
		t.Fatalf("writeTotoMCPConfig: %v", err)
	}
	if out != "" {
		t.Errorf("expected empty path when otto-memory absent, got %q", out)
	}
}

func TestWriteTotoMCPConfigBadSourceErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := writeTotoMCPConfig(filepath.Join(dir, "state"), filepath.Join(dir, "missing.json"))
	if err == nil {
		t.Fatal("expected error reading nonexistent source mcp config")
	}
}

func TestReadMCPServerNamesSorted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"zeta":{},"alpha":{},"mid":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readMCPServerNames(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alpha", "mid", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
