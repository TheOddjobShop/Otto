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

func TestLoadDerivesMemoryDefaultsFromSessionPath(t *testing.T) {
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
	wantMem := filepath.Join(dir, "memory")
	wantDB := filepath.Join(dir, "state.db")
	if cfg.MemoryDir != wantMem {
		t.Errorf("MemoryDir = %q, want %q", cfg.MemoryDir, wantMem)
	}
	if cfg.StateDBPath != wantDB {
		t.Errorf("StateDBPath = %q, want %q", cfg.StateDBPath, wantDB)
	}
}

func TestLoadHonorsExplicitMemoryPaths(t *testing.T) {
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
		"memory_dir = \"/custom/mem\"\n" +
		"state_db_path = \"/custom/state.db\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MemoryDir != "/custom/mem" || cfg.StateDBPath != "/custom/state.db" {
		t.Errorf("explicit paths not honored: mem=%q db=%q", cfg.MemoryDir, cfg.StateDBPath)
	}
}

func TestLoadDerivesEmbedDefaults(t *testing.T) {
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
	if cfg.EmbedOllamaURL != "http://localhost:11434" {
		t.Errorf("EmbedOllamaURL default = %q", cfg.EmbedOllamaURL)
	}
	if len(cfg.EmbedModels) != 2 || cfg.EmbedModels[0] != "embeddinggemma" || cfg.EmbedModels[1] != "nomic-embed-text" {
		t.Errorf("EmbedModels default = %v", cfg.EmbedModels)
	}
}

func TestLoadHonorsExplicitEmbedConfig(t *testing.T) {
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
		"embed_ollama_url = \"http://ollama:9999\"\n" +
		"embed_models = [\"only-model\"]\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EmbedOllamaURL != "http://ollama:9999" {
		t.Errorf("explicit url not honored: %q", cfg.EmbedOllamaURL)
	}
	if len(cfg.EmbedModels) != 1 || cfg.EmbedModels[0] != "only-model" {
		t.Errorf("explicit models not honored: %v", cfg.EmbedModels)
	}
}

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
		"rotate_hard_pct = 0.9\n" +
		"rotate_idle_minutes = 5\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ModelContextTokens != 100000 || cfg.RotateHardPct != 0.9 || cfg.RotateIdleMinutes != 5 {
		t.Errorf("explicit rotation config not honored: %+v", cfg)
	}
}

func TestLoadClampsOutOfRangeRotateHardPct(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	mcp := filepath.Join(dir, "mcp.json")
	for _, p := range []string{bin, mcp} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.toml")
	// rotate_hard_pct > 1.0 is meaningless (tokens/ctx never exceeds 1.0) and
	// would silently disable hard-cap rotation; Load must clamp it to the default.
	body := "telegram_bot_token = \"t\"\n" +
		"telegram_allowed_user_id = 5\n" +
		"claude_binary_path = \"" + bin + "\"\n" +
		"mcp_config_path = \"" + mcp + "\"\n" +
		"session_id_path = \"" + dir + "/session_id\"\n" +
		"rotate_hard_pct = 1.5\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.RotateHardPct != 0.85 {
		t.Errorf("RotateHardPct = %v, want 0.85 (clamped from out-of-range 1.5)", cfg.RotateHardPct)
	}
}

// TestFlushEnabledTriState covers why RotateFlush is a *bool: an absent key
// must mean "default on", while an explicit `false` must mean "off". A plain
// bool would collapse those two cases into the same zero value.
func TestFlushEnabledTriState(t *testing.T) {
	tru, fls := true, false
	cases := []struct {
		name string
		val  *bool
		want bool
	}{
		{"absent key defaults on", nil, true},
		{"explicit true", &tru, true},
		{"explicit false disables", &fls, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := &Config{RotateFlush: c.val}
			if got := cfg.FlushEnabled(); got != c.want {
				t.Errorf("FlushEnabled() = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRotateFlushParsesFromTOML pins the wiring end-to-end: an explicit
// `rotate_flush = false` in the file must survive decoding as a non-nil false,
// not be lost to the zero value.
func TestRotateFlushParsesFromTOML(t *testing.T) {
	dir := t.TempDir()
	claudeBin := filepath.Join(dir, "claude")
	mcp := filepath.Join(dir, "mcp.json")
	for _, p := range []string{claudeBin, mcp} {
		if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	body := `
telegram_bot_token = "t"
telegram_allowed_user_id = 1
claude_binary_path = "` + claudeBin + `"
mcp_config_path = "` + mcp + `"
session_id_path = "` + filepath.Join(dir, "sid") + `"
rotate_flush = false
`
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RotateFlush == nil {
		t.Fatal("rotate_flush = false decoded as nil (indistinguishable from absent)")
	}
	if cfg.FlushEnabled() {
		t.Error("FlushEnabled() = true despite rotate_flush = false")
	}
}
