//go:build unix

// Otto is a single-user Telegram bot that proxies messages to Claude Code.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/config"
	"otto/internal/permissions"
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

	session, err := claude.LoadSession(cfg.SessionIDPath)
	if err != nil {
		log.Fatalf("claude session: %v", err)
	}

	systemPrompt, err := buildSystemPrompt(cfg.SystemPromptPath, cfg.MCPConfigPath)
	if err != nil {
		log.Fatalf("system prompt: %v", err)
	}

	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		log.Fatalf("home dir: %v", err)
	}

	runner := claude.NewExecRunner(cfg.ClaudeBinaryPath, cfg.MCPConfigPath, systemPrompt, home)

	settingsPath := cfg.ClaudeSettingsPath
	if settingsPath == "" {
		settingsPath = home + "/.claude/settings.json"
	}

	h := &handler{
		bot:          bot,
		allow:        allow,
		session:      session,
		runner:       runner,
		pending:      permissions.New(64),
		settingsPath: settingsPath,
		startedAt:    time.Now(),
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

	log.Printf("otto: starting; session=%s allowed_user=%d cwd=%s sysprompt=%dB",
		session.ID(), cfg.TelegramAllowedUserID, home, len(systemPrompt))
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

// buildSystemPrompt reads the user's character/persona prompt (if configured)
// and appends an operational-context footer enumerating the MCP servers
// Claude Code is launched with. Returns "" if no prompt path was configured —
// in which case Otto won't pass --append-system-prompt to claude.
func buildSystemPrompt(promptPath, mcpConfigPath string) (string, error) {
	if promptPath == "" {
		return "", nil
	}
	body, err := os.ReadFile(promptPath)
	if err != nil {
		return "", fmt.Errorf("read system prompt %s: %w", promptPath, err)
	}
	servers, err := readMCPServerNames(mcpConfigPath)
	if err != nil {
		// Don't fail startup over a missing/malformed mcp.json — log it
		// and proceed with the persona prompt only.
		log.Printf("system prompt: %v (continuing without MCP listing)", err)
	}
	footer := operationalContextFooter(servers)
	return strings.TrimRight(string(body), "\n") + "\n\n" + footer, nil
}

func readMCPServerNames(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read mcp config %s: %w", path, err)
	}
	var v struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("parse mcp config: %w", err)
	}
	names := make([]string, 0, len(v.MCPServers))
	for k := range v.MCPServers {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

func operationalContextFooter(servers []string) string {
	var b strings.Builder
	b.WriteString("───────────────────────────────────────────────\n")
	b.WriteString("  OPERATIONAL CONTEXT\n")
	b.WriteString("───────────────────────────────────────────────\n\n")
	b.WriteString("You are running as \"Otto\" — a Telegram bot. The user texts you from their phone; your replies are delivered as plain Telegram messages.\n\n")
	b.WriteString("FORMATTING (important — read carefully):\n")
	b.WriteString("  Replies render as PLAIN TEXT on Telegram. Markdown does NOT render. Specifically:\n")
	b.WriteString("    *asterisks*, **double asterisks**, _underscores_, `backticks`, and # headers\n")
	b.WriteString("    all appear LITERALLY to the user, with the punctuation visible. Don't use them.\n\n")
	b.WriteString("  For visual structure, use:\n")
	b.WriteString("    • blank lines between sections (this is your main separator)\n")
	b.WriteString("    • plain bullet characters at the start of list items: • or - (these are just\n")
	b.WriteString("      normal characters, not markdown list syntax)\n")
	b.WriteString("    • indentation (2 spaces) to nest items\n")
	b.WriteString("    • ALL CAPS, sparingly, for occasional section labels when a list of categories\n")
	b.WriteString("      really needs headers — but prefer plain prose with blank-line separators\n\n")
	b.WriteString("  Keep replies concise — phone-screen brevity. A few short paragraphs separated by\n")
	b.WriteString("  blank lines will read better than a wall of bullets.\n\n")
	b.WriteString("ENVIRONMENT:\n")
	b.WriteString("  Running on the user's Arch Linux home server in their home directory (~/), with\n")
	b.WriteString("  full filesystem access. You can read, write, and execute commands as needed.\n\n")
	if len(servers) > 0 {
		b.WriteString("AVAILABLE MCP TOOLS:\n")
		for _, s := range servers {
			b.WriteString("  • " + s + describeServer(s) + "\n")
		}
		b.WriteString("\n")
		b.WriteString("When the user asks about email, calendar, files, notes, or anything that lives in\n")
		b.WriteString("those tools, fetch the current state via the relevant MCP rather than guessing or\n")
		b.WriteString("relying on memory. Read before writing; check before assuming.\n")
	}
	return b.String()
}

// describeServer returns a short hint after each MCP server name to help
// Claude pick the right one for a given user request. Recognized names map
// to a short capability blurb; unknown names get an empty suffix so the
// raw name still appears in the listing.
func describeServer(name string) string {
	switch {
	case name == "gdrive":
		return " — Google Drive: list, search, read, upload files"
	case strings.HasPrefix(name, "gmail-"):
		label := strings.TrimPrefix(name, "gmail-")
		return " — Gmail (" + label + " account): search, read, send messages"
	case name == "gmail":
		return " — Gmail: search, read, send, label messages"
	case name == "google-calendar":
		return " — Google Calendar: list/create events, find free time"
	case name == "notion":
		return " — Notion: search and read pages, create entries"
	}
	return ""
}
