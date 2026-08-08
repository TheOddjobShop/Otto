//go:build unix

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"otto/internal/config"
	"otto/internal/voice"
)

// Subcommand dispatch.
//
// `otto` with no subcommand is the daemon, which is what systemd and launchd
// invoke and must keep working exactly as before. Subcommands are recognized
// only as a bare first argument, so `otto -config …` still parses as flags.

const usageText = `otto — single-user Telegram bot wrapping Claude Code

Usage:
  otto [flags]           run the bot daemon (this is what the service runs)
  otto tui [flags]       run the daemon with a terminal UI and voice
  otto say <message>     hand a message to the running Otto (for scheduled work)
  otto voice-doctor      check that everything the voice stack needs is present

Flags:
  -config <path>   path to config.toml (default ~/.config/otto/config.toml)
  -tty             read messages from stdin instead of Telegram (testing)
`

// runSubcommand handles a bare first argument. Returns handled=false when the
// caller should fall through to the normal daemon path.
func runSubcommand(args []string) (handled bool, code int) {
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return false, 0
	}
	switch args[1] {
	case "tui":
		// The TUI shares the daemon's flag set; main handles it after
		// stripping the subcommand.
		return false, 0
	case "say":
		return true, runSay(args[2:])
	case "voice-doctor":
		return true, runVoiceDoctor(args[2:])
	case "help", "-h", "--help":
		fmt.Print(usageText)
		return true, 0
	default:
		fmt.Fprintf(os.Stderr, "otto: unknown command %q\n\n%s", args[1], usageText)
		return true, 2
	}
}

// runVoiceDoctor prints the voice diagnostics and exits non-zero if anything is
// outright broken.
//
// This is the answer to the one part of Otto that CI cannot test. Without it,
// "voice doesn't work" spans a dozen causes — a missing binary, a missing
// model, a model without its sidecar config, an unopenable capture device, a
// sound server that is not running — and they are indistinguishable from the
// symptom alone.
func runVoiceDoctor(args []string) int {
	stateDir := voiceStateDir(args)
	cfg := voice.DefaultConfig(stateDir)

	fmt.Printf("Otto voice diagnostics\n")
	fmt.Printf("  voice assets: %s\n\n", cfg.Dir)

	checks := voice.Diagnose(context.Background(), cfg, true)
	fmt.Print(voice.FormatChecks(checks))

	if voice.HasFailure(checks) {
		fmt.Printf("\nVoice will not work until the failures above are resolved.\n")
		return 1
	}
	return 0
}

// voiceStateDir resolves where voice assets live, preferring the configured
// state directory so the doctor inspects the same paths the daemon uses.
//
// A broken or absent config must not stop the diagnostics from running — a
// misconfigured install is exactly when someone reaches for this command — so
// any failure falls back to the conventional location.
func voiceStateDir(args []string) string {
	configPath := defaultConfigPath()
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-config" || args[i] == "--config" {
			configPath = args[i+1]
			break
		}
	}
	if cfg, err := config.Load(configPath); err == nil && cfg.StateDBPath != "" {
		return filepath.Dir(cfg.StateDBPath)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "state", "otto")
}
