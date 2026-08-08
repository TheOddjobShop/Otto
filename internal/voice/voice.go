// Package voice wraps the local whisper.cpp and piper CLIs for speech-to-text
// and text-to-speech. Everything runs on the user's own machine — no API keys,
// no per-token cost, nothing leaves the box. That mirrors the choice already
// made for embeddings (internal/embed talks to a local Ollama), so voice adds a
// capability without adding a dependency on anyone else's uptime or pricing.
//
// The package is deliberately split so the parts that can be tested without
// audio hardware are separable from the parts that cannot:
//
//	wake.go      wake-word and phrase matching   — pure, fully tested
//	sanitize.go  markdown stripping for TTS      — pure, fully tested
//	sentence.go  streaming sentence splitting    — pure, fully tested
//	stt.go       whisper.cpp subprocess          — needs the binary
//	tts.go       piper subprocess + playback     — needs the binary
//	mic.go       the sox capture device + the gate that closes it
//	client.go    VAD and the conversation state machine
//	install.go   first-run asset download        — needs the network
//	doctor.go    diagnostics for all of the above
//
// The microphone is behind an interface (mic.go's CaptureDevice), which makes
// the one behavior that matters most testable without hardware: Otto releases
// the capture device entirely while he is thinking and speaking, so he cannot
// hear his own voice through the speakers.
package voice

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Persona names the three characters that can speak. Each gets its own piper
// voice so the busy-handoff is audible: when Otto is mid-task and Toto answers
// instead, the user hears a different voice rather than the same one behaving
// oddly.
const (
	PersonaOtto = "otto"
	PersonaToto = "toto"
	PersonaToot = "toot"
)

// defaultVoiceModels maps each persona to its piper model basename.
//
// All three are "low" quality, which is not a compromise here: low-quality
// piper models are ~20 MB and synthesize a sentence in tens of milliseconds on
// CPU, and the channel is a spoken assistant reply rather than an audiobook.
// The three were chosen to be distinguishable from each other first and
// pleasant second — the whole point is that you can tell who is talking without
// being told.
var defaultVoiceModels = map[string]string{
	PersonaOtto: "en_US-danny-low",  // neutral, unhurried — the assistant
	PersonaToto: "en_US-amy-low",    // lighter and quicker — the cat
	PersonaToot: "en_US-lessac-low", // crisper, more clipped — the clerk
}

// DefaultWakeWord is what the listener waits for. See wake.go for the alias
// table that covers how ASR actually hears it.
const DefaultWakeWord = "otto"

// Config resolves every path and model the voice stack needs. Callers build one
// with DefaultConfig and may override any field from Otto's config.toml.
type Config struct {
	// Dir is where models and downloaded binaries live. Defaults to
	// <state dir>/voice, alongside state.db and the memory core, so a single
	// directory holds everything Otto keeps on disk.
	Dir string

	// WhisperModel is the absolute path to a ggml-*.bin. Resolved by
	// ResolveWhisperModel when left empty.
	WhisperModel string

	// Voices maps persona name to an absolute .onnx path. Missing entries fall
	// back to the default model for that persona.
	Voices map[string]string

	// WakeWord is the word that arms the listener.
	WakeWord string

	// RequestEndSilenceMs is how long the user must stop talking before a
	// request is considered finished and the microphone is released. Zero uses
	// defaultRequestEndSilenceMs.
	//
	// This is the number that decides whether Otto feels patient or impatient.
	// Too short and he cuts you off mid-thought; too long and every question
	// carries a dead pause before he starts working on it.
	RequestEndSilenceMs int

	// ConversationTimeoutSec is how long an answered conversation stays open
	// for a follow-up before it drops back to needing the wake word. Zero uses
	// defaultConversationTimeoutSec; negative disables the timeout, which means
	// everything said in the room after a reply goes to the model.
	ConversationTimeoutSec int
}

const (
	// defaultRequestEndSilenceMs — two seconds. Long enough to think mid
	// sentence ("remind me to… uh… call the dentist"), short enough that the
	// wait before Otto starts is not itself annoying.
	defaultRequestEndSilenceMs = 2000

	// defaultConversationTimeoutSec bounds how long follow-ups skip the wake
	// word. Half a minute is roughly how long a natural pause runs before the
	// exchange is over in practice.
	defaultConversationTimeoutSec = 30
)

// DefaultConfig returns the conventional layout under stateDir (the directory
// holding state.db). An empty stateDir falls back to ~/.local/state/otto.
func DefaultConfig(stateDir string) Config {
	if stateDir == "" {
		home, _ := os.UserHomeDir()
		stateDir = filepath.Join(home, ".local", "state", "otto")
	}
	dir := filepath.Join(stateDir, "voice")
	voices := make(map[string]string, len(defaultVoiceModels))
	for persona, model := range defaultVoiceModels {
		voices[persona] = filepath.Join(dir, model+".onnx")
	}
	return Config{
		Dir:                    dir,
		WhisperModel:           ResolveWhisperModel(dir),
		Voices:                 voices,
		WakeWord:               DefaultWakeWord,
		RequestEndSilenceMs:    defaultRequestEndSilenceMs,
		ConversationTimeoutSec: defaultConversationTimeoutSec,
	}
}

// RequestEndSilence returns the configured request endpoint, or the default.
func (c Config) RequestEndSilence() time.Duration {
	if c.RequestEndSilenceMs <= 0 {
		return defaultRequestEndSilenceMs * time.Millisecond
	}
	return time.Duration(c.RequestEndSilenceMs) * time.Millisecond
}

// ConversationTimeout returns how long an open conversation survives without
// being spoken to. A negative configured value returns zero, which the watcher
// reads as "never time out".
func (c Config) ConversationTimeout() time.Duration {
	switch {
	case c.ConversationTimeoutSec < 0:
		return 0
	case c.ConversationTimeoutSec == 0:
		return defaultConversationTimeoutSec * time.Second
	default:
		return time.Duration(c.ConversationTimeoutSec) * time.Second
	}
}

// whisperModelPreference lists ggml models best-first. Accuracy matters more
// here than it might seem: the wake word is two syllables and the closers are
// single words ("thanks", "bye"), which is exactly where a smaller model's
// error rate shows up as Otto appearing not to hear you.
//
// small.en is the download default. base and tiny are honored when already on
// disk so an existing install is never forced into a re-download.
var whisperModelPreference = []string{
	"ggml-small.en.bin",
	"ggml-base.en.bin",
	"ggml-tiny.en.bin",
}

// ResolveWhisperModel returns the best whisper model present in dir. When none
// is present it returns the path small.en *would* occupy, so the caller's error
// names a concrete missing file rather than an empty string.
func ResolveWhisperModel(dir string) string {
	for _, name := range whisperModelPreference {
		p := filepath.Join(dir, name)
		if fileExists(p) {
			return p
		}
	}
	return filepath.Join(dir, whisperModelPreference[0])
}

// VoiceFor returns the piper model path for a persona, falling back to Otto's
// voice for an unknown name. Falling back rather than erroring is deliberate:
// a mis-specified persona should sound wrong, not silently fail to speak.
func (c Config) VoiceFor(persona string) string {
	if p, ok := c.Voices[strings.ToLower(strings.TrimSpace(persona))]; ok && p != "" {
		return p
	}
	if p, ok := c.Voices[PersonaOtto]; ok && p != "" {
		return p
	}
	return filepath.Join(c.Dir, defaultVoiceModels[PersonaOtto]+".onnx")
}

// Wake returns the configured wake word, or the default when unset.
func (c Config) Wake() string {
	if w := strings.TrimSpace(c.WakeWord); w != "" {
		return w
	}
	return DefaultWakeWord
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
