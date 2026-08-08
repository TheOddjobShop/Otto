package voice

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// First-run asset installation.
//
// setup.sh installs sox and whisper.cpp, which need a package manager. Piper
// and the model files do not — they are plain downloads — so the binary can
// self-heal when launched on a machine that skipped setup, rather than failing
// with instructions the user has to go act on.
//
// Progress is printed for every download. A silent half-gigabyte fetch on first
// launch is indistinguishable from a hang, and "it froze on startup" is a much
// worse first impression than "it is downloading a speech model".

const (
	piperReleaseBase     = "https://github.com/rhasspy/piper/releases/download/2023.11.14-2/"
	phonemizeReleaseBase = "https://github.com/rhasspy/piper-phonemize/releases/download/2023.11.14-4/"
	voicesBase           = "https://huggingface.co/rhasspy/piper-voices/resolve/v1.0.0/en/en_US/"
	whisperSmallURL      = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-small.en.bin"

	// maxDownloadBytes caps any single asset. The largest legitimate download
	// is whisper small.en at ~466 MB.
	maxDownloadBytes = 700 * 1024 * 1024

	downloadTimeout = 30 * time.Minute
)

// Progress receives human-readable installation updates. The TUI wires this to
// its status line; a plain CLI run writes to stderr.
type Progress func(msg string)

func (p Progress) emit(format string, args ...any) {
	if p == nil {
		return
	}
	p(fmt.Sprintf(format, args...))
}

// StderrProgress writes progress to stderr.
func StderrProgress(msg string) { fmt.Fprintf(os.Stderr, "[voice] %s\n", msg) }

// Missing reports which assets are absent, as human-readable names. An empty
// slice means everything needed is present.
func (c Config) Missing() []string {
	var out []string
	if PiperBinary(c.Dir) == "" {
		out = append(out, "piper binary")
	}
	if !fileExists(ResolveWhisperModel(c.Dir)) {
		out = append(out, "whisper model")
	}
	for _, persona := range []string{PersonaOtto, PersonaToto, PersonaToot} {
		if !fileExists(c.VoiceFor(persona)) {
			out = append(out, persona+" voice model")
		}
	}
	return out
}

// EnsureInstalled downloads whatever is missing: the piper binary (plus its
// dylibs on macOS), the three persona voice models, and a whisper model.
//
// It deliberately does not install sox or whisper.cpp — those are system
// binaries needing a package manager, and setup.sh owns them.
func EnsureInstalled(ctx context.Context, cfg Config, prog Progress) error {
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return fmt.Errorf("create voice dir: %w", err)
	}
	if err := ensurePiper(ctx, cfg, prog); err != nil {
		return fmt.Errorf("piper: %w", err)
	}
	if err := ensureVoiceModels(ctx, cfg, prog); err != nil {
		return fmt.Errorf("voice models: %w", err)
	}
	if err := ensureWhisperModel(ctx, cfg, prog); err != nil {
		return fmt.Errorf("whisper model: %w", err)
	}
	return nil
}

// piperHome is where the piper tarball is extracted: a sibling of the voice
// model dir, matching the layout PiperBinary probes.
func piperHome(voiceDir string) string { return filepath.Dir(voiceDir) }

func ensurePiper(ctx context.Context, cfg Config, prog Progress) error {
	if PiperBinary(cfg.Dir) != "" {
		return nil
	}
	tarball, ok := piperTarballName()
	if !ok {
		return fmt.Errorf("no prebuilt piper for %s/%s — install piper manually and put it on PATH",
			runtime.GOOS, runtime.GOARCH)
	}
	root := piperHome(cfg.Dir)
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	prog.emit("downloading piper (%s)…", tarball)
	if err := downloadAndExtractTar(ctx, piperReleaseBase+tarball, root); err != nil {
		return err
	}
	bin := filepath.Join(root, "piper", "piper")
	_ = os.Chmod(bin, 0700)
	if runtime.GOOS == "darwin" {
		_ = stripQuarantine(filepath.Join(root, "piper"))
	}
	if !fileExists(bin) {
		return fmt.Errorf("piper extraction did not produce %s", bin)
	}

	// Linux piper tarballs bundle their own .so files; macOS ships them in a
	// separate piper-phonemize release, without which the binary aborts on
	// launch with a missing @rpath dylib.
	if runtime.GOOS == "darwin" {
		if err := ensurePhonemizeDylibs(ctx, root, prog); err != nil {
			return err
		}
	}
	prog.emit("piper installed")
	return nil
}

func ensurePhonemizeDylibs(ctx context.Context, root string, prog Progress) error {
	piperDir := filepath.Join(root, "piper")
	if hasSharedLibs(piperDir) {
		return nil
	}
	name, ok := piperPhonemizeTarballName()
	if !ok {
		return fmt.Errorf("no piper-phonemize build for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	prog.emit("downloading piper support libraries (%s)…", name)
	tmp, err := os.MkdirTemp("", "otto-phonemize-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := downloadAndExtractTar(ctx, phonemizeReleaseBase+name, tmp); err != nil {
		return err
	}
	libSrc := filepath.Join(tmp, "piper-phonemize", "lib")
	entries, err := os.ReadDir(libSrc)
	if err != nil {
		return fmt.Errorf("piper-phonemize lib dir missing: %w", err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".dylib") {
			continue
		}
		if err := copyPreservingSymlinks(filepath.Join(libSrc, e.Name()), filepath.Join(piperDir, e.Name())); err != nil {
			return fmt.Errorf("install %s: %w", e.Name(), err)
		}
	}
	_ = stripQuarantine(piperDir)
	if !hasSharedLibs(piperDir) {
		return fmt.Errorf("piper-phonemize extraction produced no libraries")
	}
	return nil
}

// ensureVoiceModels downloads each persona's .onnx and its sidecar .json.
// piper needs both — the JSON carries the phoneme map and sample rate — and a
// model present without its config fails at synthesis time with an error that
// does not mention the missing file.
func ensureVoiceModels(ctx context.Context, cfg Config, prog Progress) error {
	for _, persona := range []string{PersonaOtto, PersonaToto, PersonaToot} {
		model := cfg.VoiceFor(persona)
		base := strings.TrimSuffix(filepath.Base(model), ".onnx")
		speaker, quality, ok := parseVoiceName(base)
		if !ok {
			// A hand-configured voice we cannot construct a URL for. Leave it
			// to the user rather than guessing at a download path.
			if !fileExists(model) {
				return fmt.Errorf("voice model %s is missing and its name is not a recognized piper voice — download it manually", model)
			}
			continue
		}
		if !fileExists(model) {
			prog.emit("downloading %s voice (%s)…", persona, base)
			url := fmt.Sprintf("%s%s/%s/%s.onnx", voicesBase, speaker, quality, base)
			if err := downloadFile(ctx, url, model); err != nil {
				return err
			}
		}
		cfgPath := model + ".json"
		if !fileExists(cfgPath) {
			url := fmt.Sprintf("%s%s/%s/%s.onnx.json", voicesBase, speaker, quality, base)
			if err := downloadFile(ctx, url, cfgPath); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseVoiceName splits "en_US-danny-low" into speaker and quality so the
// HuggingFace path can be built. Returns ok=false for anything that is not in
// piper's naming scheme.
func parseVoiceName(base string) (speaker, quality string, ok bool) {
	parts := strings.Split(base, "-")
	if len(parts) != 3 || parts[0] != "en_US" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func ensureWhisperModel(ctx context.Context, cfg Config, prog Progress) error {
	// Honor any model already on disk — a prior install with base or tiny is
	// perfectly usable, and forcing a 466 MB re-download to upgrade it would be
	// a rude thing to do unprompted.
	for _, name := range whisperModelPreference {
		if fileExists(filepath.Join(cfg.Dir, name)) {
			return nil
		}
	}
	dst := filepath.Join(cfg.Dir, whisperModelPreference[0])
	prog.emit("downloading whisper small.en (~466 MB, one time)…")
	return downloadFile(ctx, whisperSmallURL, dst)
}

func piperTarballName() (string, bool) {
	switch runtime.GOOS + "-" + runtime.GOARCH {
	case "linux-amd64":
		return "piper_linux_x86_64.tar.gz", true
	case "linux-arm64":
		return "piper_linux_aarch64.tar.gz", true
	case "linux-arm":
		return "piper_linux_armv7l.tar.gz", true
	case "darwin-arm64":
		return "piper_macos_aarch64.tar.gz", true
	case "darwin-amd64":
		return "piper_macos_x64.tar.gz", true
	}
	return "", false
}

func piperPhonemizeTarballName() (string, bool) {
	switch runtime.GOOS + "-" + runtime.GOARCH {
	case "darwin-arm64":
		return "piper-phonemize_macos_aarch64.tar.gz", true
	case "darwin-amd64":
		return "piper-phonemize_macos_x64.tar.gz", true
	}
	return "", false
}

// ─── Download helpers ────────────────────────────────────────────────────

func httpGet(ctx context.Context, url string) (*http.Response, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("GET %s: http %d", url, resp.StatusCode)
	}
	// Cancel when the body closes, so the timeout covers the whole transfer
	// rather than just the response headers.
	resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

// downloadFile fetches url to dest atomically. Writes to a .partial sibling and
// renames, so an interrupted download can never leave a truncated model that
// piper or whisper would later fail on in a confusing way.
func downloadFile(ctx context.Context, url, dest string) error {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	tmp := dest + ".partial"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// Read one byte past the cap so an oversized asset is an explicit error
	// rather than a silent truncation.
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if n > maxDownloadBytes {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("asset %s exceeds the %d MB cap", url, maxDownloadBytes>>20)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// downloadAndExtractTar fetches a .tar.gz and extracts it into destDir,
// preserving symlinks (piper ships its libraries as symlink chains) and exec
// bits, and refusing entries that would escape destDir.
func downloadAndExtractTar(ctx context.Context, url, destDir string) error {
	resp, err := httpGet(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	gz, err := gzip.NewReader(io.LimitReader(resp.Body, maxDownloadBytes))
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(destDir, hdr.Name)
		rel, err := filepath.Rel(destDir, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			// Path traversal entry — skip rather than abort, so one hostile or
			// malformed member cannot deny the whole install.
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				return err
			}
			f, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, maxDownloadBytes)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			if hdr.Mode&0o111 != 0 {
				_ = os.Chmod(target, 0700)
			}
		}
	}
	return nil
}

func copyPreservingSymlinks(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	_ = os.Remove(dst)
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, info.Mode().Perm())
}

// stripQuarantine clears the macOS quarantine xattr so downloaded binaries can
// execute without a Gatekeeper prompt. No-op elsewhere.
func stripQuarantine(dir string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return exec.Command("xattr", "-dr", "com.apple.quarantine", dir).Run()
}
