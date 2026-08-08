package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Decoding arbitrary audio to the one format whisper accepts.
//
// Telegram voice notes are OGG/Opus. whisper.cpp reads 16 kHz mono WAV and
// nothing else, so a note has to be transcoded before it can be transcribed.
// The microphone path never needs this — sox already hands us raw PCM in the
// right shape — so decoding exists purely for audio that arrives from outside.

// decoders are the transcoders to try, in order.
//
// ffmpeg first because it handles Opus everywhere without extra packages. sox
// is the fallback and is already a dependency for capture, but its Opus support
// lives in a separate format library that distros package inconsistently — so
// it may be installed and still fail on this specific input, which is why the
// error below names the format rather than just the tool.
var decoders = []struct {
	bin  string
	args func(in, out string) []string
}{
	{"ffmpeg", func(in, out string) []string {
		return []string{"-hide_banner", "-loglevel", "error", "-y",
			"-i", in, "-ac", "1", "-ar", fmt.Sprint(sampleRate), "-f", "wav", out}
	}},
	{"sox", func(in, out string) []string {
		return []string{in, "-c", "1", "-r", fmt.Sprint(sampleRate), "-b", "16",
			"-e", "signed-integer", out}
	}},
}

// DecoderAvailable reports whether any transcoder is installed.
func DecoderAvailable() (string, bool) {
	for _, d := range decoders {
		if _, err := exec.LookPath(d.bin); err == nil {
			return d.bin, true
		}
	}
	return "", false
}

// DecodeToWAV converts encoded audio into 16 kHz mono WAV.
//
// ext is the source file extension including the dot (".oga" for a Telegram
// voice note). It matters: both decoders sniff the container from the filename,
// and handing them a bare temp name makes them guess wrong.
func DecodeToWAV(ctx context.Context, data []byte, ext string) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("decode: no audio data")
	}
	if ext == "" {
		ext = ".oga"
	}

	dir, err := os.MkdirTemp("", "otto-decode-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "in"+ext)
	outPath := filepath.Join(dir, "out.wav")
	if err := os.WriteFile(inPath, data, 0600); err != nil {
		return nil, fmt.Errorf("decode: write input: %w", err)
	}

	var lastErr error
	for _, d := range decoders {
		if _, err := exec.LookPath(d.bin); err != nil {
			continue
		}
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, d.bin, d.args(inPath, outPath)...)
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			lastErr = fmt.Errorf("%s: %w (%s)", d.bin, err, clip(stderr.String(), 200))
			// Try the next decoder: a sox without Opus support fails here while
			// ffmpeg would succeed, and vice versa on a minimal container.
			_ = os.Remove(outPath)
			continue
		}
		wav, err := os.ReadFile(outPath)
		if err != nil {
			lastErr = fmt.Errorf("%s produced no output: %w", d.bin, err)
			continue
		}
		if len(wav) == 0 {
			lastErr = fmt.Errorf("%s produced an empty file", d.bin)
			continue
		}
		return wav, nil
	}

	if lastErr == nil {
		return nil, fmt.Errorf("decode: no audio decoder installed — need ffmpeg (recommended) or sox with Opus support; run `otto voice-doctor`")
	}
	return nil, fmt.Errorf("decode %s: %w", ext, lastErr)
}
