//go:build unix

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"otto/internal/config"
	"otto/internal/tui"
	"otto/internal/voice"
)

// Front-end startup.
//
// `otto tui` runs the whole daemon with a face attached: it polls Telegram
// exactly as the service does AND owns the microphone. The UI is a surface on
// the mux, so nothing it does bypasses the handler — a spoken message takes the
// same path as a typed one, through the same allowlist, commands, pet routing,
// model router and memory.

// localFanout is the mux's local surface. It sends every reply to two places:
// the screen, and the bridge that decides whether it still needs to be spoken.
type localFanout struct {
	ui     *tui.Model
	bridge *voiceBridge
}

func (f *localFanout) Deliver(ctx context.Context, text string, isHTML bool) {
	f.ui.Deliver(ctx, text, isHTML)
	if f.bridge != nil {
		// Completes a turn that never streamed — a pet covering for a busy
		// Otto, a command reply, an error. For a streamed Otto turn this is
		// the same text already spoken and the bridge ignores it.
		f.bridge.Delivered(text, isHTML)
	}
}

// runTUI starts the terminal UI and blocks until it exits, then cancels the
// daemon so the process shuts down cleanly.
//
// Voice is best-effort by design. A missing microphone, model or binary must
// not stop the UI from running — you can still type, and the status line says
// exactly what is wrong — because a front end that refuses to start is far
// worse than one that starts without sound.
func runTUI(ctx context.Context, cancel context.CancelFunc, h *handler, mux *muxBot, cfg *config.Config, stateDir string, userID int64) {
	// Bubble Tea owns the terminal, so anything written to stderr would be
	// painted over the UI. Redirect the log to a file for the session.
	restoreLog := redirectLogToFile(stateDir)
	defer restoreLog()

	bridge := newVoiceBridge(mux, userID)
	h.voiceSink = bridge

	vc, voiceErr := startVoiceClient(ctx, cfg, stateDir, bridge)
	if voiceErr != nil {
		log.Printf("tui: voice unavailable: %v", voiceErr)
	}

	ui := tui.New(tui.Options{
		Submit:  mux,
		UserID:  userID,
		Voice:   voiceController(vc),
		Version: version,
	})
	mux.AttachSurface(&localFanout{ui: ui, bridge: bridge})
	// /new clears Otto's session from either surface; the pane on screen must
	// not outlive the context it belongs to.
	h.onSessionReset = ui.SessionReset

	if vc != nil {
		go func() {
			if err := vc.Start(ctx); err != nil {
				log.Printf("tui: voice loop exited: %v", err)
			}
		}()
		// Pre-render the canned acknowledgments in the background. First run
		// costs a second or so per phrase; every run after is a stat call.
		go func() {
			if err := vc.WarmCache(ctx); err != nil && ctx.Err() == nil {
				log.Printf("tui: warm phrase cache: %v", err)
			}
		}()
	}

	p := tea.NewProgram(ui, tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		log.Printf("tui: %v", err)
	}
	// Closing the UI ends the session: this process is Otto.
	cancel()
}

// voiceController returns the interface the UI wants, preserving a true nil so
// the UI's `Voice == nil` check works. A typed nil in an interface is not nil,
// and would make the UI try to read events from a client that does not exist.
func voiceController(vc *voice.Client) tui.VoiceController {
	if vc == nil {
		return nil
	}
	return vc
}

// startVoiceClient installs whatever is missing, then builds the listener.
func startVoiceClient(ctx context.Context, appCfg *config.Config, stateDir string, bridge *voiceBridge) (*voice.Client, error) {
	cfg := applyVoiceOverrides(voice.DefaultConfig(stateDir), appCfg)

	if missing := cfg.Missing(); len(missing) > 0 {
		// Downloads run before the UI takes the terminal, so progress is
		// visible as ordinary output. A silent half-gigabyte fetch behind a
		// splash screen is indistinguishable from a hang.
		fmt.Fprintf(os.Stderr, "Setting up voice (missing: %v)\n", missing)
		instCtx, cancel := context.WithTimeout(ctx, 40*time.Minute)
		err := voice.EnsureInstalled(instCtx, cfg, voice.StderrProgress)
		cancel()
		if err != nil {
			return nil, err
		}
		// Re-resolve: the whisper model path depends on what is now on disk.
		cfg = applyVoiceOverrides(voice.DefaultConfig(stateDir), appCfg)
	}

	if checks := voice.Diagnose(ctx, cfg, false); voice.HasFailure(checks) {
		return nil, fmt.Errorf("voice prerequisites missing:\n%s", voice.FormatChecks(checks))
	}

	return voice.NewClient(voice.ClientOptions{
		Config:    cfg,
		STT:       voice.WhisperTranscriber{Model: cfg.WhisperModel},
		TTS:       voice.PiperSpeaker{Dir: cfg.Dir},
		Responder: bridge,
		Logger:    log.Default(),
	})
}

// applyVoiceOverrides layers the optional [voice] keys from config.toml over
// the conventional defaults.
//
// Only keys that were actually set are applied: an absent key must keep the
// package's default rather than overwrite it with a zero, which for the wake
// word would mean nothing could ever wake Otto up.
func applyVoiceOverrides(cfg voice.Config, app *config.Config) voice.Config {
	if app == nil {
		return cfg
	}
	if w := strings.TrimSpace(app.VoiceWakeWord); w != "" {
		cfg.WakeWord = w
	}
	if app.VoiceEndSilenceMs > 0 {
		cfg.RequestEndSilenceMs = app.VoiceEndSilenceMs
	}
	// Negative is meaningful here — it disables the timeout — so this is the
	// one override that accepts a value below zero.
	if app.VoiceConversationTimeoutSec != 0 {
		cfg.ConversationTimeoutSec = app.VoiceConversationTimeoutSec
	}
	return cfg
}

// redirectLogToFile points the standard logger at <stateDir>/tui.log for the
// life of the UI, returning a func that restores it.
//
// Otto logs continuously — bus dispatches, rotation, the router, the voice
// state machine — and every one of those lines would otherwise be painted
// directly over the interface.
func redirectLogToFile(stateDir string) func() {
	if stateDir == "" {
		log.SetOutput(io.Discard)
		return func() { log.SetOutput(os.Stderr) }
	}
	path := filepath.Join(stateDir, "tui.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		log.SetOutput(io.Discard)
		return func() { log.SetOutput(os.Stderr) }
	}
	log.SetOutput(f)
	log.Printf("--- tui session start (version=%s) ---", version)
	return func() {
		log.SetOutput(os.Stderr)
		f.Close()
		fmt.Fprintf(os.Stderr, "otto: session log written to %s\n", path)
	}
}
