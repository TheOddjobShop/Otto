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
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"otto/internal/auth"
	"otto/internal/claude"
	"otto/internal/config"
	"otto/internal/embed"
	"otto/internal/memory"
	"otto/internal/store"
	"otto/internal/telegram"
)

// version is the build-time version string. Overridden via
// -ldflags "-X main.version=v1.2.3" in CI release builds; "dev" for
// local builds. The updater skips polling entirely when version == "dev".
var version = "dev"

func main() {
	configPath := flag.String("config", defaultConfigPath(), "path to config.toml")
	ttyMode := flag.Bool("tty", false, "test mode: read messages from stdin, write replies to stdout (no Telegram)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	allow := auth.New(cfg.TelegramAllowedUserID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var bot telegram.BotClient
	if *ttyMode {
		bot = newTTYBot(ctx, cfg.TelegramAllowedUserID, cancel)
		fmt.Fprintln(os.Stderr, "[tty] type messages and press enter; ctrl-d to exit")
	} else {
		bot, err = telegram.NewBotClient(cfg.TelegramBotToken, "https://api.telegram.org/bot%s/%s")
		if err != nil {
			log.Fatalf("telegram: %v", err)
		}
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

	// Open the conversation turn-log store. store.Open creates the DB file but
	// not its parent directory, so ensure the directory exists first.
	if err := os.MkdirAll(filepath.Dir(cfg.StateDBPath), 0700); err != nil {
		log.Fatalf("state db dir: %v", err)
	}
	memStore, err := store.Open(cfg.StateDBPath)
	if err != nil {
		log.Fatalf("open state db: %v", err)
	}
	defer memStore.Close()

	// Curated memory core, injected into every persona's prompt and written
	// via the otto-memory MCP server.
	memCore := memory.NewCore(cfg.MemoryDir, memCapChars, userCapChars)

	// Embedder for semantic memory. Degrades to keyword search if Ollama is
	// unavailable (the chain returns an error and callers fall back to FTS).
	embedder := embed.NewOllamaChain(cfg.EmbedOllamaURL, cfg.EmbedModels)

	// Toto: separate runner with a scoped MCP config that exposes ONLY
	// otto-memory (so Toto can use forward_to_otto and session_search,
	// but cannot reach gmail/notion/etc.). Toto's --model, --allowedTools,
	// and per-call system prompt are set inside Toto.Reply.
	totoPersona, err := readTotoPersona(cfg.TotoPersonaPath)
	if err != nil {
		log.Fatalf("toto persona: %v", err)
	}
	stateDir := filepath.Dir(cfg.StateDBPath)
	petMCPPath, err := writeScopedPetMCPConfig(stateDir, cfg.MCPConfigPath)
	if err != nil {
		log.Fatalf("pet mcp config: %v", err)
	}
	if petMCPPath == "" {
		log.Printf("pets: no otto-memory entry found in %s — pet bus tools disabled", cfg.MCPConfigPath)
	}
	totoRunner := claude.NewExecRunner(cfg.ClaudeBinaryPath, petMCPPath, "", home)
	totoSessionPath := cfg.TotoSessionIDPath
	if totoSessionPath == "" {
		// Default: sibling of the Otto session file. Keeps Toto's
		// conversation memory persistent without requiring an extra
		// config field for users on older config.toml templates.
		totoSessionPath = cfg.SessionIDPath + "_toto"
	}
	totoSession, err := claude.LoadSession(totoSessionPath)
	if err != nil {
		log.Fatalf("toto session: %v", err)
	}

	// Toot mirrors Toto's wiring: own runner with no MCP, own session,
	// own persona file. Per-call --model, --effort, and --allowedTools
	// are set inside Toot.reply (chat mode); --disallowedTools "*" is
	// set inside Toot.Announce (announcement mode, all tools blocked).
	tootPersona, err := readTootPersona(cfg.TootPersonaPath)
	if err != nil {
		log.Fatalf("toot persona: %v", err)
	}
	// Toot reuses the scoped pet MCP config (only otto-memory) so he has
	// access to the bus tools — message_toto / forward_to_otto / session_search.
	// The persona keeps him from touching them in plain chat mode; in bus
	// mode the per-call BUS CONTEXT block tells him to reply via
	// message_<sender>.
	tootRunner := claude.NewExecRunner(cfg.ClaudeBinaryPath, petMCPPath, "", home)
	tootSessionPath := cfg.TootSessionIDPath
	if tootSessionPath == "" {
		tootSessionPath = cfg.SessionIDPath + "_toot"
	}
	tootSession, err := claude.LoadSession(tootSessionPath)
	if err != nil {
		log.Fatalf("toot session: %v", err)
	}

	toto := &Toto{
		bot:      bot,
		runner:   totoRunner,
		session:  totoSession,
		persona:  totoPersona,
		mem:      memCore,
		store:    memStore,
		embedder: embedder,
	}
	toot := &Toot{
		bot:      bot,
		runner:   tootRunner,
		session:  tootSession,
		persona:  tootPersona,
		mem:      memCore,
		store:    memStore,
		embedder: embedder,
		version:  version,
	}

	h := &handler{
		bot:              bot,
		allow:            allow,
		session:          session,
		runner:           runner,
		startedAt:        time.Now(),
		otto:             newOttoState(),
		toto:             toto,
		mem:              memCore,
		store:            memStore,
		embedder:         embedder,
		baseSystemPrompt: systemPrompt,
		// Pet registry — addressed messages route here before Otto.
		// Adding a new pet later: implement Pet, append to this list.
		pets: newPetRegistry(toto, toot),
		// Per-turn model router: Haiku for chat, Opus for coding tasks.
		// A cheap Haiku one-shot decides; failures fall back to Haiku.
		classifier: &execClassifier{binary: cfg.ClaudeBinaryPath, workDir: home},
		// Pets rotate their own sessions on the idle window too.
		petRotators: []petRotator{toto, toot},
	}

	h.rotate = rotateConfig{
		ctxTokens:  cfg.ModelContextTokens,
		hard:       cfg.RotateHardPct,
		idleWindow: time.Duration(cfg.RotateIdleMinutes) * time.Minute,
	}

	// Toto can see what Otto's up to so he can answer "what's otto
	// doing?" honestly when addressed directly.
	toto.ottoStatus = h.otto.Snapshot

	// Construct the updater BEFORE binding Toot's hooks: Go method values
	// capture the receiver at evaluation time, so `h.updater.Pending`
	// would otherwise bind to a nil *updater and panic on first call.
	h.updater = newUpdater(toot, cfg.TelegramAllowedUserID, version, cfg.StateDBPath)

	// Boot-back-online ping: if the previous process wrote an
	// install-confirm marker (i.e. we're booting after a successful
	// /update), have Toot tell the user we're up. Runs in a goroutine
	// with a small grace period so the message lands after the bot has
	// settled rather than racing the first log lines.
	go maybeSendBootConfirm(ctx, toot, cfg.TelegramAllowedUserID, cfg.StateDBPath, version, bootConfirmGrace)

	// Toot can see whether an update is pending and trigger the
	// install when the user authorizes it in chat. The trigger
	// callback is the same goroutine /update spawns.
	toot.pendingUpdate = h.updater.Pending
	toot.triggerUpdate = h.runUpdate
	toot.checkNow = h.updater.CheckNow

	go h.updater.Run(ctx)

	// busDrainWG tracks the runBusDrain goroutine so the shutdown sequence
	// can wait for it to exit before memStore.Close() fires. runBusDrain
	// calls store.DequeueAll and (via dispatchBusMessage → logTurn) also
	// store.AppendTurn; closing the store while the goroutine is mid-call
	// would corrupt in-flight writes. runRotator and runUpdater do NOT
	// touch memStore directly, so they do not need the same tracking.
	var busDrainWG sync.WaitGroup
	busDrainWG.Add(1)
	go func() {
		defer busDrainWG.Done()
		// Bus drain reads inbox rows and dispatches them. Otto's
		// message_toto / message_toot tools (in otto-memory) rely on Otto's
		// mcp.json already exposing the "otto-memory" server entry — setup.sh
		// writes that entry, so no wiring is needed here. If a user's mcp.json
		// somehow lacks it, that's a config issue (the tools simply won't
		// appear in Otto's tool list); no code change makes them work.
		h.runBusDrain(ctx)
	}()
	go h.runRotator(ctx)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		s := <-sigs
		log.Printf("otto: received %s, shutting down", s)
		cancel()
	}()

	log.Printf("otto: starting; session=%s toto_session=%s toot_session=%s allowed_user=%d cwd=%s sysprompt=%dB toto_persona=%dB toot_persona=%dB memory_dir=%s state_db=%s embed=%s",
		session.ID(), totoSession.ID(), tootSession.ID(), cfg.TelegramAllowedUserID, home, len(systemPrompt), len(totoPersona), len(tootPersona), cfg.MemoryDir, cfg.StateDBPath, embedder.Name())
	if err := h.runPollingLoop(ctx); err != nil && err != context.Canceled {
		log.Fatalf("polling loop: %v", err)
	}
	// Drain in-flight dispatches so Otto/Toto goroutines get a chance to
	// finish their Telegram replies before the process exits.
	h.WaitDispatches()
	// Cancel the context if the signal handler has not already done so,
	// then wait for runBusDrain to exit before memStore.Close() fires.
	// runBusDrain calls DequeueAll and (via logTurn) AppendTurn; closing
	// the store while it is mid-call would corrupt in-flight writes.
	cancel()
	busDrainWG.Wait()
	// runBusDrain's final iteration may have spawned dispatch goroutines
	// after the WaitDispatches above returned; wait again so no bus turn
	// is still writing to the store when the deferred Close fires.
	h.WaitDispatches()
	log.Printf("otto: stopped")
}

// readPersonaFile returns the contents of a persona file, or empty string
// if the path is empty (the persona runs with Claude Code's defaults).
// A missing-but-configured file is a hard error so misconfiguration is
// noisy at startup rather than silently disabling the character. name is
// used only in the error message (e.g. "toto", "toot").
func readPersonaFile(path, name string) (string, error) {
	if path == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s persona %s: %w", name, path, err)
	}
	return strings.TrimRight(string(body), "\n"), nil
}

// readTotoPersona wraps readPersonaFile for the Toto character.
func readTotoPersona(path string) (string, error) { return readPersonaFile(path, "toto") }

// readTootPersona wraps readPersonaFile for the Toot character.
func readTootPersona(path string) (string, error) { return readPersonaFile(path, "toot") }

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

// writeScopedPetMCPConfig produces a pet-scoped mcp.json containing ONLY
// the "otto-memory" server entry pulled from the user's full mcp.json,
// and writes it under stateDir with perms 0600. Returns the written path,
// or "" if the source mcp.json has no otto-memory entry (in which case
// the caller should run the pets in no-tools mode).
//
// Scoping matters: Otto's full mcp.json exposes Gmail, Notion, Calendar,
// etc. The pets are chat personas, not assistants — handing them those
// servers would let the model exfiltrate or mutate data outside their
// remit. We give them exactly the MCP they need to talk to each other
// and to Otto via the inbox bus.
//
// Both Toto and Toot read this same file: their per-call --allowedTools
// allowlists already differ to encode each pet's narrower toolset, so a
// shared scoped mcp.json is sufficient and avoids two near-identical
// generated files on disk.
func writeScopedPetMCPConfig(stateDir, ottoMCPPath string) (string, error) {
	data, err := os.ReadFile(ottoMCPPath)
	if err != nil {
		return "", fmt.Errorf("read otto mcp config %s: %w", ottoMCPPath, err)
	}
	var v struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("parse otto mcp config: %w", err)
	}
	entry, ok := v.MCPServers["otto-memory"]
	if !ok {
		return "", nil
	}
	scoped := struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}{
		MCPServers: map[string]json.RawMessage{"otto-memory": entry},
	}
	out, err := json.MarshalIndent(scoped, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode toto mcp config: %w", err)
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return "", fmt.Errorf("ensure state dir %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, "pet-mcp.json")
	if err := os.WriteFile(path, out, 0600); err != nil {
		return "", fmt.Errorf("write pet mcp config %s: %w", path, err)
	}
	return path, nil
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
