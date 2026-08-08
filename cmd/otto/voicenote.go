//go:build unix

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"otto/internal/telegram"
	"otto/internal/voice"
)

// Telegram voice notes.
//
// The microphone button is the natural way to reach Otto while walking or
// cooking, and it needs none of the TUI: the note is transcribed locally and
// then treated exactly as if it had been typed. Everything downstream —
// commands, pet routing, the model router, memory — sees ordinary text.
//
// The reply comes back as text, not audio. The user is looking at Telegram, so
// they can read; speaking back would be slower and less skimmable. Speech
// output belongs to the TUI, where there is no screen in play.

const (
	// maxVoiceNoteSeconds bounds what Otto will transcribe. whisper runs at
	// roughly real time on CPU for small.en, so a long note would hold the
	// dispatch goroutine for minutes while the user waits with no feedback.
	// Five minutes is far beyond any plausible spoken request.
	maxVoiceNoteSeconds = 300

	// voiceNoteTimeout bounds download plus decode plus transcription.
	voiceNoteTimeout = 4 * time.Minute
)

// resolveVoiceNote turns a voice note into text, returning the transcript and
// whether dispatch should continue.
//
// Every failure path replies to the user and returns ok=false. Silence would be
// the worst outcome here: from the user's side they sent a message and got
// nothing, with no way to tell whether Otto is thinking, broken, or ignoring
// them.
func (h *handler) resolveVoiceNote(ctx context.Context, u telegram.Update) (string, bool) {
	if h.voiceSTT == nil {
		h.replyVoiceError(ctx, u.ChatID,
			"I can't transcribe voice notes — speech-to-text isn't set up on this machine. Run otto voice-doctor to see what's missing.")
		return "", false
	}
	if u.VoiceSeconds > maxVoiceNoteSeconds {
		h.replyVoiceError(ctx, u.ChatID, fmt.Sprintf(
			"That voice note is %ds — I only transcribe up to %ds. Could you send a shorter one, or type it?",
			u.VoiceSeconds, maxVoiceNoteSeconds))
		return "", false
	}

	ctx, cancel := context.WithTimeout(ctx, voiceNoteTimeout)
	defer cancel()

	data, _, err := h.bot.DownloadFile(ctx, u.VoiceFileID)
	if err != nil {
		log.Printf("voice note: download: %v", err)
		h.replyVoiceError(ctx, u.ChatID, "I couldn't download that voice note. Mind trying again?")
		return "", false
	}

	// Telegram serves voice notes as OGG/Opus regardless of the sending
	// platform, so the extension is fixed rather than sniffed.
	decode := h.voiceDecode
	if decode == nil {
		decode = voice.DecodeToWAV
	}
	wav, err := decode(ctx, data, ".oga")
	if err != nil {
		log.Printf("voice note: decode: %v", err)
		h.replyVoiceError(ctx, u.ChatID,
			"I couldn't decode that voice note. Installing ffmpeg would fix it — otto voice-doctor has the details.")
		return "", false
	}

	text, err := h.voiceSTT.Transcribe(ctx, wav)
	if err != nil {
		log.Printf("voice note: transcribe: %v", err)
		h.replyVoiceError(ctx, u.ChatID, "I couldn't make out that voice note. Could you try again, or type it?")
		return "", false
	}

	text = strings.TrimSpace(text)
	if text == "" {
		// Distinguished from a failure on purpose: the pipeline worked and
		// there was simply nothing intelligible, which is the user's cue to
		// speak up rather than to go debugging.
		h.replyVoiceError(ctx, u.ChatID, "I didn't catch anything in that one — all I got was silence.")
		return "", false
	}

	log.Printf("voice note: %ds → %q", u.VoiceSeconds, truncate(text, 80))
	return text, true
}

// replyVoiceError sends a voice-note failure to the user.
func (h *handler) replyVoiceError(ctx context.Context, chatID int64, msg string) {
	if err := telegram.SendChunked(ctx, h.bot, chatID, msg); err != nil {
		log.Printf("voice note: send error reply: %v", err)
	}
}

// newVoiceTranscriber builds the transcriber used for Telegram voice notes, or
// nil when the assets are not present.
//
// Returning nil rather than an error keeps voice notes strictly additive: an
// install without whisper behaves exactly as Otto did before, and the only
// difference is the explanatory reply if someone sends one.
func newVoiceTranscriber(stateDir string) voice.Transcriber {
	cfg := voice.DefaultConfig(stateDir)
	if voice.WhisperBinary() == "" {
		log.Printf("voice notes: whisper CLI not found — voice notes will be declined")
		return nil
	}
	model := cfg.WhisperModel
	if model == "" {
		model = voice.ResolveWhisperModel(cfg.Dir)
	}
	if _, err := os.Stat(model); err != nil {
		log.Printf("voice notes: whisper model missing at %s — voice notes will be declined", filepath.Base(model))
		return nil
	}
	log.Printf("voice notes: enabled (model=%s)", filepath.Base(model))
	return voice.WhisperTranscriber{Model: model}
}
