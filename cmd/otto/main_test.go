//go:build unix

package main

import (
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
