package artanim

import (
	"strings"
	"testing"
)

// countLines counts rendered rows, tolerating a trailing newline.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}

// stripANSI removes color escapes so geometry can be measured.
func stripANSI(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func TestRenderLightbulbFillsRequestedHeight(t *testing.T) {
	for _, size := range []struct{ w, h int }{{80, 24}, {120, 40}, {40, 14}} {
		got := RenderLightbulbScaled(size.w, size.h, 0, 1.0)
		if n := countLines(got); n != size.h {
			t.Errorf("%dx%d rendered %d lines, want %d", size.w, size.h, n, size.h)
		}
		for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
			if w := len([]rune(stripANSI(line))); w > size.w {
				t.Errorf("line %d is %d cells wide, exceeds the %d-column canvas", i, w, size.w)
			}
		}
	}
}

// The spin must actually change the image, or the animation loop is burning
// frames for nothing.
func TestRenderLightbulbRotates(t *testing.T) {
	a := RenderLightbulbScaled(80, 24, 0, 1.0)
	b := RenderLightbulbScaled(80, 24, 1.2, 1.0)
	if a == b {
		t.Error("rotating the bulb produced an identical frame")
	}
}

// Same inputs must produce the same frame — the renderer is called every tick
// and any hidden state would show up as flicker.
func TestRenderLightbulbIsDeterministic(t *testing.T) {
	a := RenderLightbulbScaled(80, 24, 0.7, 1.0)
	b := RenderLightbulbScaled(80, 24, 0.7, 1.0)
	if a != b {
		t.Error("identical inputs produced different frames")
	}
}

func TestRenderLightbulbDrawsSomething(t *testing.T) {
	got := stripANSI(RenderLightbulbScaled(80, 24, 0, 1.0))
	if strings.TrimSpace(got) == "" {
		t.Fatal("rendered an empty canvas")
	}
	// The shading ramp should be in use, not a single flat character.
	shadeCount := 0
	for _, r := range []rune("░▒▓█") {
		if strings.ContainsRune(got, r) {
			shadeCount++
		}
	}
	if shadeCount < 2 {
		t.Errorf("only %d shade levels present; the lighting model is not doing anything", shadeCount)
	}
}

// A pathologically small terminal must not panic or write out of bounds.
func TestRenderLightbulbTinyCanvas(t *testing.T) {
	for _, size := range []struct{ w, h int }{{1, 1}, {5, 2}, {10, 3}} {
		got := RenderLightbulbScaled(size.w, size.h, 0.5, 0.3)
		if n := countLines(got); n != size.h {
			t.Errorf("%dx%d rendered %d lines, want %d", size.w, size.h, n, size.h)
		}
	}
}

func TestRenderSiriBars(t *testing.T) {
	got := RenderSiriBars(80, 4, 0.8, 1.0)
	if n := countLines(got); n != 4 {
		t.Errorf("rendered %d rows, want 4", n)
	}
	if strings.TrimSpace(stripANSI(got)) == "" {
		t.Error("bars at amplitude 0.8 rendered nothing")
	}
}

// At rest the cluster should still be faintly visible, so the UI does not look
// dead when Otto is simply idle.
func TestRenderSiriBarsIdleStillShowsStems(t *testing.T) {
	got := stripANSI(RenderSiriBars(80, 4, 0.05, 0))
	if !strings.Contains(got, "▁") {
		t.Error("idle bars should render dim stems rather than nothing at all")
	}
}

func TestRenderSiriBarsRejectsTinyCanvas(t *testing.T) {
	if got := RenderSiriBars(5, 4, 0.5, 0); got != "" {
		t.Errorf("a too-narrow canvas should render nothing, got %q", got)
	}
	if got := RenderSiriBars(80, 1, 0.5, 0); got != "" {
		t.Errorf("a too-short canvas should render nothing, got %q", got)
	}
}

func TestRenderSiriBarsClampsAmplitude(t *testing.T) {
	// Out-of-range amplitudes must not escape the canvas.
	for _, amp := range []float64{-5, 0, 1, 42} {
		got := RenderSiriBars(80, 4, amp, 0.5)
		if n := countLines(got); n != 4 {
			t.Errorf("amplitude %v rendered %d rows, want 4", amp, n)
		}
	}
}

func TestRenderBootRevealsProgressively(t *testing.T) {
	early := stripANSI(RenderBoot(90, 30, BootProgress{Rows: 1, SubRow: 10}))
	late := stripANSI(RenderBoot(90, 30, BootProgress{Rows: 10, PostRows: 4}))

	earlyBlocks := strings.Count(early, "█")
	lateBlocks := strings.Count(late, "█")
	if lateBlocks <= earlyBlocks {
		t.Errorf("later progress drew %d blocks, not more than the earlier %d", lateBlocks, earlyBlocks)
	}
	if !strings.Contains(late, "SYSTEM READY") {
		t.Error("status lines should appear once the reveal completes")
	}
}

func TestRenderBootMentionsOttoSubsystems(t *testing.T) {
	got := stripANSI(RenderBoot(90, 30, BootProgress{Rows: 10, PostRows: 4}))
	for _, want := range []string{"MEMORY CORE", "BUS", "WATCHDOG"} {
		if !strings.Contains(got, want) {
			t.Errorf("boot status should mention %q, got:\n%s", want, got)
		}
	}
}

func TestGradientColorSpansStops(t *testing.T) {
	first := gradientColor(0, 10)
	last := gradientColor(9, 10)
	if first == last {
		t.Error("the gradient should change across rows")
	}
	if first != gradStops[0] {
		t.Errorf("row 0 = %s, want the first stop %s", first, gradStops[0])
	}
	// A single row must not divide by zero.
	if got := gradientColor(0, 1); got != gradStops[0] {
		t.Errorf("single-row gradient = %s, want %s", got, gradStops[0])
	}
}

func TestLerpHexClamps(t *testing.T) {
	if got := lerpHex("#000000", "#ffffff", -1); got != "#000000" {
		t.Errorf("t<0 = %s, want the start color", got)
	}
	if got := lerpHex("#000000", "#ffffff", 2); got != "#ffffff" {
		t.Errorf("t>1 = %s, want the end color", got)
	}
	if got := lerpHex("#000000", "#ffffff", 0.5); got != "#7f7f7f" {
		t.Errorf("midpoint = %s, want #7f7f7f", got)
	}
}

func TestAnsi24(t *testing.T) {
	if got := ansi24("#ff8000"); got != "\x1b[38;2;255;128;0m" {
		t.Errorf("ansi24 = %q", got)
	}
	if got := ansi24("nonsense"); got != "" {
		t.Errorf("malformed hex should yield no escape, got %q", got)
	}
}
