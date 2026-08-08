package voice

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Config ──────────────────────────────────────────────────────────────

func TestDefaultConfigLayout(t *testing.T) {
	cfg := DefaultConfig("/state/otto")
	if cfg.Dir != filepath.Join("/state/otto", "voice") {
		t.Errorf("Dir = %q, want the voice subdirectory of the state dir", cfg.Dir)
	}
	if cfg.Wake() != DefaultWakeWord {
		t.Errorf("Wake = %q, want %q", cfg.Wake(), DefaultWakeWord)
	}
	// Each persona must resolve to a distinct model, or the whole point of
	// per-character voices is lost.
	seen := map[string]string{}
	for _, p := range []string{PersonaOtto, PersonaToto, PersonaToot} {
		v := cfg.VoiceFor(p)
		if v == "" {
			t.Fatalf("persona %q has no voice", p)
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("persona %q shares voice %q with %q", p, v, prev)
		}
		seen[v] = p
	}
}

func TestVoiceForUnknownPersonaFallsBackToOtto(t *testing.T) {
	cfg := DefaultConfig("/state/otto")
	if got, want := cfg.VoiceFor("nobody"), cfg.VoiceFor(PersonaOtto); got != want {
		t.Errorf("unknown persona resolved to %q, want Otto's voice %q", got, want)
	}
}

func TestVoiceForIsCaseInsensitive(t *testing.T) {
	cfg := DefaultConfig("/state/otto")
	if got, want := cfg.VoiceFor("TOTO"), cfg.VoiceFor(PersonaToto); got != want {
		t.Errorf("VoiceFor(\"TOTO\") = %q, want %q", got, want)
	}
}

func TestWakeFallsBackWhenBlank(t *testing.T) {
	cfg := Config{WakeWord: "   "}
	if cfg.Wake() != DefaultWakeWord {
		t.Errorf("blank wake word should fall back to %q, got %q", DefaultWakeWord, cfg.Wake())
	}
}

// An existing base or tiny model must be honored rather than triggering a
// 466 MB re-download of small.
func TestResolveWhisperModelPrefersBestPresent(t *testing.T) {
	dir := t.TempDir()

	// Nothing present: name the file small.en *would* occupy, so the caller's
	// error can point at something concrete.
	if got := ResolveWhisperModel(dir); filepath.Base(got) != "ggml-small.en.bin" {
		t.Errorf("with nothing on disk got %q, want the small.en path", got)
	}

	writeFile(t, filepath.Join(dir, "ggml-tiny.en.bin"))
	if got := ResolveWhisperModel(dir); filepath.Base(got) != "ggml-tiny.en.bin" {
		t.Errorf("got %q, want the tiny model that is actually present", got)
	}

	writeFile(t, filepath.Join(dir, "ggml-base.en.bin"))
	if got := ResolveWhisperModel(dir); filepath.Base(got) != "ggml-base.en.bin" {
		t.Errorf("got %q, want base to outrank tiny", got)
	}

	writeFile(t, filepath.Join(dir, "ggml-small.en.bin"))
	if got := ResolveWhisperModel(dir); filepath.Base(got) != "ggml-small.en.bin" {
		t.Errorf("got %q, want small to outrank base", got)
	}
}

func TestMissingListsEveryAbsentAsset(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	missing := cfg.Missing()
	for _, want := range []string{"whisper model", "otto voice model"} {
		if !containsSubstring(missing, want) {
			t.Errorf("Missing() = %q, want it to include %q", missing, want)
		}
	}
}

func TestParseVoiceName(t *testing.T) {
	speaker, quality, ok := parseVoiceName("en_US-danny-low")
	if !ok || speaker != "danny" || quality != "low" {
		t.Errorf("parseVoiceName = (%q,%q,%v), want (danny,low,true)", speaker, quality, ok)
	}
	for _, bad := range []string{"danny", "en_GB-alan-low", "en_US-danny", "en_US-danny-low-extra"} {
		if _, _, ok := parseVoiceName(bad); ok {
			t.Errorf("parseVoiceName(%q) reported ok; want a rejection so no URL is guessed", bad)
		}
	}
}

// ─── Sanitize ────────────────────────────────────────────────────────────

func TestSanitizeForTTS(t *testing.T) {
	tests := []struct{ name, in, want string }{
		{"bold", "That is **very** important", "That is very important"},
		{"italic", "That is *very* important", "That is very important"},
		{"inline code", "Run `make test` now", "Run make test now"},
		{"heading", "# Summary\nAll good", "Summary\nAll good"},
		{"link", "See [the docs](https://example.com) for more", "See the docs for more"},
		{"bullets", "- one\n- two", "one\ntwo"},
		{"numbered", "1. one\n2. two", "one\ntwo"},
		{"blockquote", "> quoted text", "quoted text"},
		{"rule", "before\n---\nafter", "before\n\nafter"},
		{"emoji", "done ✅ and shipped 🚀", "done  and shipped"},
		{"plain untouched", "Nothing to strip here.", "Nothing to strip here."},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeForTTS(tc.in); got != tc.want {
				t.Errorf("SanitizeForTTS(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeForTTSStripsCodeFences(t *testing.T) {
	got := SanitizeForTTS("Here:\n```go\nfmt.Println(\"hi\")\n```\nthat's it")
	if strings.Contains(got, "```") {
		t.Errorf("code fence survived: %q", got)
	}
	if !strings.Contains(got, "fmt.Println") {
		t.Errorf("fence content should be kept, got %q", got)
	}
}

// Running twice must not corrupt already-clean text — the sanitizer sits on a
// path where it may be applied more than once.
func TestSanitizeForTTSIsIdempotent(t *testing.T) {
	in := "**bold** and `code` and a [link](http://x)"
	once := SanitizeForTTS(in)
	if twice := SanitizeForTTS(once); twice != once {
		t.Errorf("not idempotent: %q then %q", once, twice)
	}
}

// ─── Transcript cleaning ─────────────────────────────────────────────────

// whisper emits its non-speech annotations as ordinary text, so without
// stripping them a silent room produces "[BLANK_AUDIO]" and every one of those
// becomes a wake-word check against literal noise.
func TestCleanTranscriptDropsNoiseAnnotations(t *testing.T) {
	for _, in := range []string{
		"[BLANK_AUDIO]", "  [blank_audio]  ", "[SILENCE]", "(music)", "[Applause]",
	} {
		if got := CleanTranscript(in); got != "" {
			t.Errorf("CleanTranscript(%q) = %q, want empty", in, got)
		}
	}
}

func TestCleanTranscriptKeepsSpeech(t *testing.T) {
	if got := CleanTranscript("  otto what's the weather  \n"); got != "otto what's the weather" {
		t.Errorf("got %q, want the trimmed transcript", got)
	}
	// Mixed noise and speech keeps the speech.
	got := CleanTranscript("[BLANK_AUDIO] otto do the thing")
	if got != "otto do the thing" {
		t.Errorf("got %q, want the annotation removed and speech kept", got)
	}
}

// ─── Audio ───────────────────────────────────────────────────────────────

func TestRMS(t *testing.T) {
	if got := rms(nil); got != 0 {
		t.Errorf("rms(nil) = %v, want 0", got)
	}
	silence := make([]int16, 100)
	if got := rms(silence); got != 0 {
		t.Errorf("rms(silence) = %v, want 0", got)
	}
	loud := make([]int16, 100)
	for i := range loud {
		loud[i] = 32767
	}
	if got := rms(loud); got < 0.99 || got > 1.0 {
		t.Errorf("rms(full scale) = %v, want ~1.0", got)
	}
}

// The WAV header has to be exactly right or whisper rejects the file with an
// error that says nothing useful about which field was wrong.
func TestPCMToWavHeader(t *testing.T) {
	samples := []int16{0, 1, -1, 32767}
	wav := pcmToWav(samples, sampleRate)

	if len(wav) != 44+len(samples)*2 {
		t.Fatalf("length = %d, want %d (44-byte header + PCM)", len(wav), 44+len(samples)*2)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[12:16]) != "fmt " {
		t.Errorf("bad chunk identifiers: %q %q %q", wav[0:4], wav[8:12], wav[12:16])
	}
	if string(wav[36:40]) != "data" {
		t.Errorf("data chunk id = %q, want \"data\"", wav[36:40])
	}
	if got := le32(wav[24:28]); got != sampleRate {
		t.Errorf("sample rate = %d, want %d", got, sampleRate)
	}
	if got := le16(wav[22:24]); got != 1 {
		t.Errorf("channels = %d, want 1 (mono)", got)
	}
	if got := le16(wav[34:36]); got != 16 {
		t.Errorf("bits per sample = %d, want 16", got)
	}
	if got := le32(wav[28:32]); got != sampleRate*2 {
		t.Errorf("byte rate = %d, want %d", got, sampleRate*2)
	}
}

func TestFrameRingKeepsMostRecent(t *testing.T) {
	r := newFrameRing(2)
	r.push([]int16{1})
	r.push([]int16{2})
	r.push([]int16{3})
	got := r.drain()
	if len(got) != 2 || got[0][0] != 2 || got[1][0] != 3 {
		t.Errorf("ring kept %v, want the two most recent frames", got)
	}
	if len(r.drain()) != 0 {
		t.Error("drain should have emptied the ring")
	}
}

func TestFrameRingReset(t *testing.T) {
	r := newFrameRing(3)
	r.push([]int16{1})
	r.reset()
	if len(r.drain()) != 0 {
		t.Error("reset should discard buffered frames")
	}
}

// ─── Cache ───────────────────────────────────────────────────────────────

// Keying on voice as well as text is what stops Toto's line being served in
// Otto's voice by whichever character happened to say it first.
func TestCacheKeyIncludesVoice(t *testing.T) {
	c := NewCache(t.TempDir())
	a := c.Path("en_US-danny-low.onnx", "Yes?")
	b := c.Path("en_US-amy-low.onnx", "Yes?")
	if a == b {
		t.Error("same phrase in different voices must not share a cache entry")
	}
	if c.Path("en_US-danny-low.onnx", "Yes?") != a {
		t.Error("cache path must be stable for the same inputs")
	}
}

// The basename is used rather than the full path, so relocating the voice
// directory does not silently invalidate every cached phrase.
func TestCacheKeyIgnoresModelDirectory(t *testing.T) {
	c := NewCache(t.TempDir())
	a := c.Path("/one/place/en_US-danny-low.onnx", "Yes?")
	b := c.Path("/somewhere/else/en_US-danny-low.onnx", "Yes?")
	if a != b {
		t.Error("cache key should depend on the model basename, not its directory")
	}
}

func TestCacheRoundTrip(t *testing.T) {
	c := NewCache(t.TempDir())
	model, phrase := "en_US-danny-low.onnx", "Yes?"

	if got := c.Get(model, phrase); got != nil {
		t.Errorf("empty cache returned %v, want nil", got)
	}
	want := []byte("fake wav bytes")
	if err := c.Put(model, phrase, want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := c.Get(model, phrase); string(got) != string(want) {
		t.Errorf("Get = %q, want %q", got, want)
	}
	// A different voice must still miss.
	if got := c.Get("en_US-amy-low.onnx", phrase); got != nil {
		t.Error("a different voice should miss the cache")
	}
}

func TestCacheWarmRendersEveryPhraseOnce(t *testing.T) {
	c := NewCache(t.TempDir())
	sp := &countingSpeaker{}
	if err := c.Warm(context.Background(), sp, "en_US-danny-low.onnx"); err != nil {
		t.Fatalf("Warm: %v", err)
	}
	if sp.calls != len(CannedPhrases()) {
		t.Errorf("rendered %d phrases, want %d", sp.calls, len(CannedPhrases()))
	}
	// Warming again must be nearly free — this runs on every startup.
	before := sp.calls
	if err := c.Warm(context.Background(), sp, "en_US-danny-low.onnx"); err != nil {
		t.Fatalf("second Warm: %v", err)
	}
	if sp.calls != before {
		t.Errorf("second warm made %d extra calls, want 0", sp.calls-before)
	}
}

func TestCacheWarmStopsOnCancelledContext(t *testing.T) {
	c := NewCache(t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Warm(ctx, &countingSpeaker{}, "en_US-danny-low.onnx"); err == nil {
		t.Error("Warm should return the context error rather than rendering")
	}
}

// ─── Doctor ──────────────────────────────────────────────────────────────

// Diagnostics must never panic and must always report on every component, since
// the whole point is a legible first failure.
func TestDiagnoseCoversEveryComponent(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	checks := Diagnose(context.Background(), cfg, false)
	for _, want := range []string{"sox", "whisper-cli", "piper", "audio playback", "whisper model", "otto voice", "toto voice", "toot voice"} {
		found := false
		for _, c := range checks {
			if c.Name == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("Diagnose did not report on %q", want)
		}
	}
}

// A missing pet voice degrades that character to Otto's voice; a missing Otto
// voice means nothing can speak. They must not be reported at the same
// severity.
func TestDiagnoseGradesPetVoicesAsWarnings(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	checks := Diagnose(context.Background(), cfg, false)
	for _, c := range checks {
		switch c.Name {
		case "otto voice":
			if c.Status != CheckFail {
				t.Errorf("missing Otto voice graded %v, want FAIL", c.Status)
			}
		case "toto voice", "toot voice":
			if c.Status != CheckWarn {
				t.Errorf("missing %s graded %v, want warn", c.Name, c.Status)
			}
		}
	}
}

// Every non-OK check must carry an actionable fix — a diagnostic that only says
// something is broken is barely better than the original symptom.
func TestDiagnoseProblemsCarryFixes(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	for _, c := range Diagnose(context.Background(), cfg, false) {
		if c.Status != CheckOK && strings.TrimSpace(c.Fix) == "" {
			t.Errorf("check %q is %v but offers no fix", c.Name, c.Status)
		}
	}
}

func TestFormatChecks(t *testing.T) {
	out := FormatChecks([]Check{
		{Name: "sox", Status: CheckOK, Detail: "/usr/bin/sox"},
		{Name: "piper", Status: CheckFail, Detail: "not installed", Fix: "run ./setup.sh"},
	})
	for _, want := range []string{"sox", "piper", "not installed", "To fix:", "run ./setup.sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("FormatChecks output missing %q:\n%s", want, out)
		}
	}
}

func TestFormatChecksAllGood(t *testing.T) {
	out := FormatChecks([]Check{{Name: "sox", Status: CheckOK, Detail: "/usr/bin/sox"}})
	if !strings.Contains(out, "Everything voice needs is present") {
		t.Errorf("clean run should say so, got:\n%s", out)
	}
}

func TestHasFailureIgnoresWarnings(t *testing.T) {
	if HasFailure([]Check{{Status: CheckOK}, {Status: CheckWarn}}) {
		t.Error("warnings alone must not count as failure")
	}
	if !HasFailure([]Check{{Status: CheckOK}, {Status: CheckFail}}) {
		t.Error("a failure must be reported")
	}
}

// ─── Helpers ─────────────────────────────────────────────────────────────

type countingSpeaker struct{ calls int }

func (s *countingSpeaker) Speak(ctx context.Context, text, model string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls++
	return []byte("wav:" + model + ":" + text), nil
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
}

func containsSubstring(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

func le16(b []byte) int { return int(b[0]) | int(b[1])<<8 }
func le32(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 | int(b[3])<<24 }
