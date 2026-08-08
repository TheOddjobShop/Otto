package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// whisperBinaries are the names whisper.cpp's CLI ships under. Upstream renamed
// `main` to `whisper-cli` in 2024, and distro packages disagree about which to
// install, so both are probed rather than assuming either.
var whisperBinaries = []string{"whisper-cli", "whisper"}

// WhisperBinary returns the whisper CLI to invoke, or "" when none is on PATH.
func WhisperBinary() string {
	for _, name := range whisperBinaries {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

// Transcriber turns WAV audio into text. It exists as an interface so the
// utterance state machine can be tested with a stub — the whole pipeline above
// this line is testable without a microphone or a model, and that is the point.
type Transcriber interface {
	Transcribe(ctx context.Context, wav []byte) (string, error)
}

// WhisperTranscriber runs whisper.cpp as a subprocess.
type WhisperTranscriber struct {
	// Model is the path to a ggml-*.bin.
	Model string
	// Binary overrides the resolved whisper CLI. Empty means look it up.
	Binary string
}

// Transcribe writes wav to a temp file and runs whisper over it, returning the
// recognized text.
//
// The context bounds the subprocess: an utterance that somehow wedges whisper
// must not wedge the listener, because the listener is the only thing that can
// hear the user ask it to stop.
func (w WhisperTranscriber) Transcribe(ctx context.Context, wav []byte) (string, error) {
	bin := w.Binary
	if bin == "" {
		bin = WhisperBinary()
	}
	if bin == "" {
		return "", fmt.Errorf("whisper CLI not found on PATH (looked for %s) — run ./setup.sh or `otto voice-doctor`",
			strings.Join(whisperBinaries, ", "))
	}
	if _, err := os.Stat(w.Model); err != nil {
		return "", fmt.Errorf("whisper model missing: %s — run `otto voice-doctor`", w.Model)
	}

	tmpDir, err := os.MkdirTemp("", "otto-whisper-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	inPath := filepath.Join(tmpDir, "in.wav")
	if err := os.WriteFile(inPath, wav, 0600); err != nil {
		return "", fmt.Errorf("write wav: %w", err)
	}
	outBase := filepath.Join(tmpDir, "out")

	cmd := exec.CommandContext(ctx, bin,
		"-m", w.Model,
		"-f", inPath,
		"-otxt",
		"-of", outBase,
		"--no-prints",
		"--language", "en",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("whisper failed: %w (stderr: %s)", err, clip(stderr.String(), 300))
	}

	data, err := os.ReadFile(outBase + ".txt")
	if err != nil {
		return "", fmt.Errorf("read whisper output: %w", err)
	}
	return CleanTranscript(string(data)), nil
}

// bracketNoise are whisper's annotations for non-speech audio. They are emitted
// as ordinary text, so without stripping them a silent room transcribes to
// "[BLANK_AUDIO]" and every one of those becomes a wake-word check against
// literal noise.
var bracketNoise = []string{
	"[blank_audio]", "[silence]", "[music]", "[sound]", "[noise]",
	"(blank_audio)", "(silence)", "(music)",
	"[inaudible]", "[applause]", "[laughter]",
}

// CleanTranscript normalizes whisper output: trims whitespace and drops
// non-speech annotations. Returns "" when nothing but noise was recognized, so
// callers can treat "heard nothing" as a single empty-string case.
func CleanTranscript(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	for _, marker := range bracketNoise {
		lower = strings.ReplaceAll(lower, marker, " ")
	}
	// Rebuild from the cleaned lowercase form only when something was removed;
	// otherwise keep the original casing, which the model reads better.
	if strings.TrimSpace(lower) == "" {
		return ""
	}
	for _, marker := range bracketNoise {
		s = replaceFold(s, marker)
	}
	return strings.Join(strings.Fields(s), " ")
}

// replaceFold removes every case-insensitive occurrence of marker from s.
func replaceFold(s, marker string) string {
	for {
		i := strings.Index(strings.ToLower(s), marker)
		if i < 0 {
			return s
		}
		s = s[:i] + " " + s[i+len(marker):]
	}
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
