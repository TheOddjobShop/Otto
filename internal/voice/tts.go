package voice

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// Speaker turns text into WAV audio. An interface so the speaking path is
// testable without piper installed, mirroring claude.Runner.
type Speaker interface {
	Speak(ctx context.Context, text, model string) ([]byte, error)
}

// PiperSpeaker runs piper as a subprocess.
type PiperSpeaker struct {
	// Dir is the voice home; piper is looked for at <Dir>/../piper/piper
	// before falling back to PATH.
	Dir string
	// Binary overrides binary resolution entirely.
	Binary string
}

// PiperBinary resolves the piper executable: the locally-installed copy first
// (that is what EnsureInstalled produces), then PATH for a distro package.
func PiperBinary(dir string) string {
	local := filepath.Join(filepath.Dir(dir), "piper", "piper")
	if st, err := os.Stat(local); err == nil && st.Mode()&0o100 != 0 {
		return local
	}
	if p, err := exec.LookPath("piper"); err == nil {
		return p
	}
	return ""
}

// Speak synthesizes text with the given piper model and returns WAV bytes.
func (p PiperSpeaker) Speak(ctx context.Context, text, model string) ([]byte, error) {
	bin := p.Binary
	if bin == "" {
		bin = PiperBinary(p.Dir)
	}
	if bin == "" {
		return nil, fmt.Errorf("piper not found — run ./setup.sh or `otto voice-doctor`")
	}
	if _, err := os.Stat(model); err != nil {
		return nil, fmt.Errorf("piper voice model missing: %s — run `otto voice-doctor`", model)
	}
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("piper: nothing to speak")
	}

	tmp, err := os.CreateTemp("", "otto-piper-*.wav")
	if err != nil {
		return nil, err
	}
	outPath := tmp.Name()
	tmp.Close()
	defer os.Remove(outPath)

	cmd := exec.CommandContext(ctx, bin, "--model", model, "--output_file", outPath)
	cmd.Env = piperEnv(bin)
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("piper failed: %w (stderr: %s)", err, clip(stderr.String(), 300))
	}
	return os.ReadFile(outPath)
}

// piperEnv points the dynamic loader at piper's bundled shared libraries.
//
// Both platforms need this for the same reason and via different variables: the
// release tarballs ship their own libespeak-ng and libpiper_phonemize next to
// the binary, but the baked-in rpath searches system directories that will not
// contain them. On Linux that surfaces as "error while loading shared
// libraries: libespeak-ng.so.1"; on macOS as "Library not loaded:
// @rpath/libespeak-ng.1.dylib".
//
// A piper found on PATH is left alone — a distro package has its libraries
// installed properly and we do not know where they live.
func piperEnv(bin string) []string {
	env := os.Environ()
	dir := filepath.Dir(bin)
	if dir == "" || dir == "." {
		return env
	}
	// Only prepend for a bundled install (piper sitting beside its libs).
	if !hasSharedLibs(dir) {
		return env
	}
	for _, key := range []string{"LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH", "DYLD_FALLBACK_LIBRARY_PATH"} {
		if existing := os.Getenv(key); existing != "" {
			env = append(env, key+"="+dir+string(os.PathListSeparator)+existing)
			continue
		}
		env = append(env, key+"="+dir)
	}
	return env
}

// hasSharedLibs reports whether dir contains bundled .so/.dylib files.
func hasSharedLibs(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		n := e.Name()
		if strings.HasSuffix(n, ".dylib") || strings.Contains(n, ".so") {
			return true
		}
	}
	return false
}

// ─── Playback ────────────────────────────────────────────────────────────

// playerCandidates are audio players in preference order.
//
// Linux comes first because that is the deployment target: paplay for
// PipeWire/PulseAudio (which is what a modern Arch desktop runs), aplay for
// bare ALSA, then sox's `play` as the portable fallback — sox is already a
// dependency for capture, so it is guaranteed present even when neither sound
// server tool is. afplay is last and macOS-only, kept because development
// happens there.
var playerCandidates = []struct {
	name string
	args func(path string) []string
}{
	{"paplay", func(p string) []string { return []string{p} }},
	{"aplay", func(p string) []string { return []string{"-q", p} }},
	{"play", func(p string) []string { return []string{"-q", p} }},
	{"afplay", func(p string) []string { return []string{p} }},
}

// ResolvePlayer returns the first available audio player and its argument
// builder, or ok=false when none is installed.
func ResolvePlayer() (name string, args func(string) []string, ok bool) {
	for _, c := range playerCandidates {
		if _, err := exec.LookPath(c.name); err == nil {
			return c.name, c.args, true
		}
	}
	return "", nil, false
}

// Player plays WAV audio and can be interrupted mid-utterance.
type Player struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

// Play writes wav to a temp file and plays it to completion. Interrupt kills an
// in-flight playback.
//
// A killed process is reported as success, not error: Interrupt is how a
// keyboard mute stops a reply, and surfacing that as a playback fault would put
// a spurious error on screen every time the user tells Otto to be quiet.
func (p *Player) Play(ctx context.Context, wav []byte) error {
	name, buildArgs, ok := ResolvePlayer()
	if !ok {
		return fmt.Errorf("no audio player found (need paplay, aplay, play or afplay)")
	}
	tmp, err := os.CreateTemp("", "otto-reply-*.wav")
	if err != nil {
		return err
	}
	path := tmp.Name()
	_, writeErr := tmp.Write(wav)
	closeErr := tmp.Close()
	defer os.Remove(path)
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}

	cmd := exec.CommandContext(ctx, name, buildArgs(path)...)
	cmd.Stdout = nil
	cmd.Stderr = nil

	p.mu.Lock()
	p.cmd = cmd
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.cmd = nil
		p.mu.Unlock()
	}()

	err = cmd.Run()
	if err != nil && cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == -1 {
		// Killed by Interrupt (or ctx) rather than a genuine failure.
		return nil
	}
	return err
}

// Interrupt stops any in-flight playback. No-op when nothing is playing.
func (p *Player) Interrupt() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
}
