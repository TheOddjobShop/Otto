// Package config loads Otto's runtime configuration from a TOML file.
package config

import (
	"fmt"
	"os"
	"path/filepath"

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
	// MemoryDir holds the curated-memory files USER.md and MEMORY.md that are
	// injected into every prompt. Defaults to <dir of session_id_path>/memory.
	MemoryDir string `toml:"memory_dir"`
	// StateDBPath is the SQLite database holding the conversation turn log
	// (for session_search). Defaults to <dir of session_id_path>/state.db.
	StateDBPath string `toml:"state_db_path"`
	// EmbedOllamaURL is the base URL of the local Ollama server used for
	// semantic-memory embeddings. Default http://localhost:11434.
	EmbedOllamaURL string `toml:"embed_ollama_url"`
	// EmbedModels is the ordered list of Ollama embedding models to try
	// (first healthy wins). Default ["embeddinggemma", "nomic-embed-text"].
	EmbedModels []string `toml:"embed_models"`
	// ModelContextTokens is Otto's model context window, used as the denominator
	// for rotation thresholds. Default 200000.
	ModelContextTokens int `toml:"model_context_tokens"`
	// RotateHardPct: at this fraction of context, a continuously-active session
	// rotates at the next free tick regardless of how recently the user spoke
	// (safety cap for sessions that never go idle). Default 0.85.
	RotateHardPct float64 `toml:"rotate_hard_pct"`
	// RotateIdleMinutes: minutes of user silence after which the session is
	// cleared regardless of size — the periodic "reset on inactivity". Default 15.
	RotateIdleMinutes int `toml:"rotate_idle_minutes"`
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: decode %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	base := filepath.Dir(cfg.SessionIDPath)
	if cfg.MemoryDir == "" {
		cfg.MemoryDir = filepath.Join(base, "memory")
	}
	if cfg.StateDBPath == "" {
		cfg.StateDBPath = filepath.Join(base, "state.db")
	}
	if cfg.TootSessionIDPath == "" {
		cfg.TootSessionIDPath = cfg.SessionIDPath + "_toot"
	}
	if cfg.EmbedOllamaURL == "" {
		cfg.EmbedOllamaURL = "http://localhost:11434"
	}
	if len(cfg.EmbedModels) == 0 {
		cfg.EmbedModels = []string{"embeddinggemma", "nomic-embed-text"}
	}
	if cfg.ModelContextTokens <= 0 {
		cfg.ModelContextTokens = 200000
	}
	// Clamp to (0, 1]: the hard cap is compared against tokens/ctxTokens, a
	// ratio that can never exceed 1.0, so a value > 1.0 would silently disable
	// hard-cap rotation entirely.
	if cfg.RotateHardPct <= 0 || cfg.RotateHardPct > 1.0 {
		cfg.RotateHardPct = 0.85
	}
	if cfg.RotateIdleMinutes <= 0 {
		cfg.RotateIdleMinutes = 15
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
