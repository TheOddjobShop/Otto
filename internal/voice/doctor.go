package voice

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Diagnostics.
//
// The audio path is the one part of Otto that cannot be tested in CI — there is
// no microphone on a runner, and the development machine is not the deployment
// platform. That makes a good first failure message unusually valuable: without
// one, "voice doesn't work" is indistinguishable from a dozen causes spanning
// missing binaries, missing models, an unopenable capture device, and a sound
// server that is not running.
//
// Every check reports what it looked for and what to do about it.

// CheckStatus is the outcome of one diagnostic.
type CheckStatus int

const (
	// CheckOK means the component is present and usable.
	CheckOK CheckStatus = iota
	// CheckWarn means voice will work but in a degraded way.
	CheckWarn
	// CheckFail means voice cannot work until this is fixed.
	CheckFail
)

func (s CheckStatus) String() string {
	switch s {
	case CheckOK:
		return "ok"
	case CheckWarn:
		return "warn"
	default:
		return "FAIL"
	}
}

// Check is one diagnostic result.
type Check struct {
	Name   string
	Status CheckStatus
	Detail string
	// Fix is the concrete command or action that resolves a warn/fail.
	Fix string
}

// Diagnose runs every check and returns the results in a stable order.
//
// probeMic controls whether the microphone is actually opened. Opening it is
// the only check that has a side effect and the only one that can hang, so it
// is opt-in: `otto voice-doctor` passes true, a startup self-check does not.
func Diagnose(ctx context.Context, cfg Config, probeMic bool) []Check {
	var checks []Check

	checks = append(checks, checkBinary("sox", "microphone capture",
		"pacman -S sox   (or: apt install sox / brew install sox)"))

	if bin := WhisperBinary(); bin != "" {
		checks = append(checks, Check{Name: "whisper-cli", Status: CheckOK, Detail: bin})
	} else {
		checks = append(checks, Check{
			Name:   "whisper-cli",
			Status: CheckFail,
			Detail: "not found on PATH (looked for " + strings.Join(whisperBinaries, ", ") + ")",
			Fix:    "pacman -S whisper.cpp   (or build from github.com/ggerganov/whisper.cpp)",
		})
	}

	if bin := PiperBinary(cfg.Dir); bin != "" {
		checks = append(checks, Check{Name: "piper", Status: CheckOK, Detail: bin})
	} else {
		checks = append(checks, Check{
			Name:   "piper",
			Status: CheckFail,
			Detail: "not installed",
			Fix:    "run `otto tui` once to auto-install, or ./setup.sh",
		})
	}

	// Playback.
	if name, _, ok := ResolvePlayer(); ok {
		checks = append(checks, Check{Name: "audio playback", Status: CheckOK, Detail: name})
	} else {
		checks = append(checks, Check{
			Name:   "audio playback",
			Status: CheckFail,
			Detail: "no player found (tried paplay, aplay, play, afplay)",
			Fix:    "pacman -S libpulse   (paplay), or alsa-utils (aplay) — sox's `play` also works",
		})
	}

	// Whisper model.
	model := cfg.WhisperModel
	if model == "" {
		model = ResolveWhisperModel(cfg.Dir)
	}
	if fileExists(model) {
		checks = append(checks, Check{
			Name:   "whisper model",
			Status: CheckOK,
			Detail: fmt.Sprintf("%s (%s)", filepath.Base(model), humanSize(model)),
		})
	} else {
		checks = append(checks, Check{
			Name:   "whisper model",
			Status: CheckFail,
			Detail: "missing: " + model,
			Fix:    "run `otto tui` once to auto-download (~466 MB)",
		})
	}

	// Per-persona voice models. Otto's is required; a missing pet voice only
	// costs that character their own sound, so it is a warning.
	for _, persona := range []string{PersonaOtto, PersonaToto, PersonaToot} {
		path := cfg.VoiceFor(persona)
		name := persona + " voice"
		switch {
		case fileExists(path) && fileExists(path+".json"):
			checks = append(checks, Check{Name: name, Status: CheckOK, Detail: filepath.Base(path)})
		case fileExists(path):
			// piper needs the sidecar config for phonemes and sample rate; the
			// error it raises without one does not name the missing file.
			checks = append(checks, Check{
				Name: name, Status: CheckFail,
				Detail: "model present but its .onnx.json config is missing",
				Fix:    "delete " + path + " and re-run `otto tui` to fetch both",
			})
		case persona == PersonaOtto:
			checks = append(checks, Check{
				Name: name, Status: CheckFail,
				Detail: "missing: " + filepath.Base(path),
				Fix:    "run `otto tui` once to auto-download",
			})
		default:
			checks = append(checks, Check{
				Name: name, Status: CheckWarn,
				Detail: "missing: " + filepath.Base(path) + " — this character will speak in Otto's voice",
				Fix:    "run `otto tui` once to auto-download",
			})
		}
	}

	if probeMic {
		checks = append(checks, probeMicrophone(ctx))
	}
	return checks
}

// probeMicrophone opens the default capture device briefly and confirms samples
// actually arrive.
//
// Merely starting sox is not evidence of anything: it exits non-zero later, or
// streams silence forever, when the default device is wrong. Requiring real
// bytes distinguishes "no microphone" from "a microphone that produces
// nothing", which need different fixes.
func probeMicrophone(ctx context.Context) Check {
	if _, err := exec.LookPath("sox"); err != nil {
		return Check{Name: "microphone", Status: CheckFail, Detail: "skipped — sox not installed"}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sox", "-q", "-d", "-c", "1",
		"-r", fmt.Sprint(sampleRate), "-b", "16", "-e", "signed-integer", "-L",
		"-t", "raw", "-")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Check{Name: "microphone", Status: CheckFail, Detail: err.Error()}
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Check{
			Name: "microphone", Status: CheckFail,
			Detail: "could not start sox: " + err.Error(),
			Fix:    "check that a capture device exists: arecord -l",
		}
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()

	buf := make([]byte, frameSamples*2)
	read := make(chan int, 1)
	go func() {
		n, _ := io_ReadFull(stdout, buf)
		read <- n
	}()

	select {
	case n := <-read:
		if n < len(buf) {
			return Check{
				Name: "microphone", Status: CheckFail,
				Detail: fmt.Sprintf("capture device produced only %d of %d bytes; sox said: %s",
					n, len(buf), clip(stderr.String(), 160)),
				Fix: "check the default input: arecord -l, or set AUDIODEV / PulseAudio default source",
			}
		}
		return Check{Name: "microphone", Status: CheckOK,
			Detail: fmt.Sprintf("captured %d bytes at %d Hz", n, sampleRate)}
	case <-ctx.Done():
		return Check{
			Name: "microphone", Status: CheckFail,
			Detail: "no audio within 3s — the device opened but produced nothing",
			Fix:    "check the default input device and that it is not muted at the OS level",
		}
	}
}

// io_ReadFull is io.ReadFull, wrapped so the partial count survives an error —
// which is exactly the signal that distinguishes a dead device from a slow one.
func io_ReadFull(r interface{ Read([]byte) (int, error) }, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func checkBinary(name, purpose, fix string) Check {
	if p, err := exec.LookPath(name); err == nil {
		return Check{Name: name, Status: CheckOK, Detail: p}
	}
	return Check{
		Name:   name,
		Status: CheckFail,
		Detail: "not found on PATH — needed for " + purpose,
		Fix:    fix,
	}
}

func humanSize(path string) string {
	st, err := os.Stat(path)
	if err != nil {
		return "?"
	}
	n := st.Size()
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", n>>10)
	}
}

// FormatChecks renders diagnostics as aligned text, worst news last so the
// thing to act on is closest to the prompt.
func FormatChecks(checks []Check) string {
	width := 0
	for _, c := range checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var b strings.Builder
	var problems []Check
	for _, c := range checks {
		fmt.Fprintf(&b, "  %-*s  %-4s  %s\n", width, c.Name, c.Status, c.Detail)
		if c.Status != CheckOK {
			problems = append(problems, c)
		}
	}
	if len(problems) == 0 {
		b.WriteString("\nEverything voice needs is present.\n")
		return b.String()
	}
	b.WriteString("\nTo fix:\n")
	for _, c := range problems {
		if c.Fix == "" {
			continue
		}
		fmt.Fprintf(&b, "  %s — %s\n", c.Name, c.Fix)
	}
	return b.String()
}

// HasFailure reports whether any check failed outright (warnings excluded).
func HasFailure(checks []Check) bool {
	for _, c := range checks {
		if c.Status == CheckFail {
			return true
		}
	}
	return false
}
