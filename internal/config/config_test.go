package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	claudeBin := filepath.Join(dir, "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	mcpConfig := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpConfig, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	contents := fmt.Sprintf(`
telegram_bot_token = "tg-token"
telegram_allowed_user_id = 12345
notion_api_key = "secret_test"
claude_binary_path = %q
mcp_config_path = %q
session_id_path = "/tmp/otto-test-session"
`, claudeBin, mcpConfig)
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
notion_api_key = "secret_test"
claude_binary_path = "/usr/bin/claude"
mcp_config_path = "/tmp/mcp.json"
session_id_path = "/tmp/sid"
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing telegram_bot_token, got nil")
	}
	if !strings.Contains(err.Error(), "telegram_bot_token") {
		t.Errorf("expected error about telegram_bot_token, got: %v", err)
	}
}

func TestLoadFileNotFound(t *testing.T) {
	if _, err := Load("/nonexistent/config.toml"); err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestLoadMissingClaudeBinary(t *testing.T) {
	dir := t.TempDir()
	mcpConfig := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpConfig, []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	missingBin := filepath.Join(dir, "does-not-exist-claude")
	contents := fmt.Sprintf(`
telegram_bot_token = "tg-token"
telegram_allowed_user_id = 12345
notion_api_key = "secret_test"
claude_binary_path = %q
mcp_config_path = %q
session_id_path = "/tmp/otto-test-session"
`, missingBin, mcpConfig)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing claude_binary_path, got nil")
	}
	if !strings.Contains(err.Error(), "claude_binary_path") {
		t.Errorf("expected error mentioning claude_binary_path, got: %v", err)
	}
}

func TestLoadMissingMCPConfig(t *testing.T) {
	dir := t.TempDir()
	claudeBin := filepath.Join(dir, "claude")
	if err := os.WriteFile(claudeBin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.toml")
	missingMCP := filepath.Join(dir, "does-not-exist-mcp.json")
	contents := fmt.Sprintf(`
telegram_bot_token = "tg-token"
telegram_allowed_user_id = 12345
notion_api_key = "secret_test"
claude_binary_path = %q
mcp_config_path = %q
session_id_path = "/tmp/otto-test-session"
`, claudeBin, missingMCP)
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing mcp_config_path, got nil")
	}
	if !strings.Contains(err.Error(), "mcp_config_path") {
		t.Errorf("expected error mentioning mcp_config_path, got: %v", err)
	}
}
