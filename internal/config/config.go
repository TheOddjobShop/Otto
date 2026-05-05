// Package config loads Otto's runtime configuration from a TOML file.
package config

import (
	"fmt"
	"os"

	"github.com/BurntSushi/toml"
)

type Config struct {
	TelegramBotToken      string `toml:"telegram_bot_token"`
	TelegramAllowedUserID int64  `toml:"telegram_allowed_user_id"`
	ClaudeBinaryPath      string `toml:"claude_binary_path"`
	MCPConfigPath         string `toml:"mcp_config_path"`
	SessionIDPath         string `toml:"session_id_path"`
	// SystemPromptPath optionally points to a Markdown file appended to
	// Claude Code's built-in system prompt via --append-system-prompt.
	// Empty means no append (Claude Code's defaults stand alone).
	SystemPromptPath string `toml:"system_prompt_path"`
	// TotoSessionIDPath is where the secondary "Toto" persona persists its
	// own conversation session ID. Toto is the lightweight cat-themed
	// stand-in that replies while Otto is busy on a long-running task.
	// Required when Toto is enabled (which is the default); empty disables
	// the Toto fallback entirely.
	TotoSessionIDPath string `toml:"toto_session_id_path"`
	// TotoPersonaPath optionally points to a Markdown file appended to
	// Toto's built-in system prompt via --append-system-prompt. Mirrors
	// SystemPromptPath but for the Toto persona.
	TotoPersonaPath string `toml:"toto_persona_path"`
	// TootSessionIDPath is where the "Toot" owl persona persists its
	// own conversation session ID. Toot is the release-notes courier
	// that announces new versions when the updater detects them.
	// Defaults to <session_id_path>_toot when empty.
	TootSessionIDPath string `toml:"toot_session_id_path"`
	// TootPersonaPath optionally points to a Markdown file appended to
	// Toot's built-in system prompt via --append-system-prompt. Mirrors
	// TotoPersonaPath but for the Toot owl persona.
	TootPersonaPath string `toml:"toot_persona_path"`
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
	if _, err := os.Stat(c.ClaudeBinaryPath); err != nil {
		return fmt.Errorf("config: claude_binary_path %q does not exist: %w", c.ClaudeBinaryPath, err)
	}
	if _, err := os.Stat(c.MCPConfigPath); err != nil {
		return fmt.Errorf("config: mcp_config_path %q does not exist: %w", c.MCPConfigPath, err)
	}
	return nil
}
