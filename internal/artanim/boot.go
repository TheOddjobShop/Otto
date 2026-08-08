package artanim

import (
	"fmt"
	"strings"
)

// Big block letters for the boot splash. 10 rows tall, 12 cols per
// letter, 2-col gutter. OTTO total width: 4*12 + 3*2 = 54.
var ottoFont = []string{
	"   ██████     ████████████  ████████████    ██████   ",
	"  ██    ██    ████████████  ████████████   ██    ██  ",
	" ██      ██        ██            ██       ██      ██ ",
	"██        ██       ██            ██      ██        ██",
	"██        ██       ██            ██      ██        ██",
	"██        ██       ██            ██      ██        ██",
	"██        ██       ██            ██      ██        ██",
	" ██      ██        ██            ██       ██      ██ ",
	"  ██    ██         ██            ██        ██    ██  ",
	"   ██████          ██            ██         ██████   ",
}

const ottoWidth = 53 // visible width of each ottoFont line

// Gradient stops: warm red at the top, amber through the middle,
// yellow at the bottom — feels like a filament flaring to life.
var gradStops = []string{
	"#ef4444", // red-500
	"#f97316", // orange-500
	"#f59e0b", // amber-500
	"#fbbf24", // amber-400
}

// ottoDim is used for the trailing scanline cursor while a row loads.
var ottoDim = "#7f1d1d" // red-900

// BootProgress describes what portion of the boot animation is visible.
// Rows is how many full OTTO rows have finished loading (0..len(ottoFont)).
// SubRow is the within-row scanline offset (0..cols), used to paint the
// currently-loading row character-by-character like an old CRT.
type BootProgress struct {
	Rows     int
	SubRow   int
	PostRows int // rows drawn after OTTO (status lines / cursor)
	Blink    bool
}

// lerpHex linearly interpolates between two #rrggbb strings, returning
// a new #rrggbb at fraction t in [0,1].
func lerpHex(a, b string, t float64) string {
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	var ar, ag, ab, br, bg, bb int
	fmt.Sscanf(a, "#%02x%02x%02x", &ar, &ag, &ab)
	fmt.Sscanf(b, "#%02x%02x%02x", &br, &bg, &bb)
	r := int(float64(ar) + (float64(br)-float64(ar))*t)
	g := int(float64(ag) + (float64(bg)-float64(ag))*t)
	bl := int(float64(ab) + (float64(bb)-float64(ab))*t)
	return fmt.Sprintf("#%02x%02x%02x", r, g, bl)
}

// gradientColor returns the color for a row at index i of total n.
// Samples gradStops as a multi-stop gradient.
func gradientColor(i, n int) string {
	if n <= 1 {
		return gradStops[0]
	}
	// Place i in [0,1] across the stops.
	t := float64(i) / float64(n-1)
	// Find which segment we're in.
	segs := len(gradStops) - 1
	segF := t * float64(segs)
	seg := int(segF)
	if seg >= segs {
		seg = segs - 1
	}
	inner := segF - float64(seg)
	return lerpHex(gradStops[seg], gradStops[seg+1], inner)
}

// RenderBoot renders the boot splash at the given progress. Returns an
// ANSI-colored string centered for a terminal of size w×h.
func RenderBoot(w, h int, p BootProgress) string {
	artH := len(ottoFont)
	reset := "\x1b[0m"

	// Pre-compute the color for each OTTO row.
	rowColors := make([]string, artH)
	for i := range rowColors {
		rowColors[i] = ansi24(gradientColor(i, artH))
	}
	dim := ansi24(ottoDim)

	// Compose art lines with partial-row reveal.
	artLines := make([]string, artH)
	for i := 0; i < artH; i++ {
		switch {
		case i < p.Rows:
			artLines[i] = rowColors[i] + ottoFont[i] + reset
		case i == p.Rows:
			full := ottoFont[i]
			if p.SubRow >= len(full) {
				artLines[i] = rowColors[i] + full + reset
			} else {
				bright := full[:p.SubRow]
				head := ""
				if p.SubRow < len(full) {
					head = dim + "█" + reset
				}
				artLines[i] = rowColors[i] + bright + reset + head
			}
		default:
			artLines[i] = ""
		}
	}

	// Status lines that fade in after OTTO is done.
	statusColor := ansi24("#9ca3af") // cool gray for status
	statusLines := []string{
		"",
		"SYSTEM READY",
		"MEMORY CORE LOADED  ·  BUS DRAINING  ·  WATCHDOG ARMED",
		"LISTENING",
	}
	shown := make([]string, 0, len(statusLines))
	for i := 0; i < p.PostRows && i < len(statusLines); i++ {
		if statusLines[i] == "" {
			shown = append(shown, "")
			continue
		}
		shown = append(shown, statusColor+statusLines[i]+reset)
	}

	// Blinking cursor at the bottom of whatever's shown.
	cursor := ""
	if p.Rows >= artH && p.Blink {
		cursor = rowColors[artH-1] + "█" + reset
	} else if p.Rows >= artH {
		cursor = " "
	}

	// Credit line at the very bottom. Rendered as an OSC 8 hyperlink
	// when the terminal supports it, plain text otherwise.
	creditText := "made by justin06lee.dev"
	creditURL := "https://justin06lee.dev"
	creditColor := ansi24("#6b7280") // zinc-500 — subdued
	credit := creditColor +
		"\x1b]8;;" + creditURL + "\x1b\\" +
		creditText +
		"\x1b]8;;\x1b\\" +
		reset

	// Total content height for vertical centering.
	contentH := artH + 2 + len(shown) + 1 // OTTO + gap + status + cursor
	padTop := (h - contentH - 3) / 2      // reserve 3 rows at bottom for credit
	if padTop < 0 {
		padTop = 0
	}

	var b strings.Builder
	for i := 0; i < padTop; i++ {
		b.WriteByte('\n')
	}
	for _, line := range artLines {
		b.WriteString(centerVisible(line, w, ottoWidth))
		b.WriteByte('\n')
	}
	// Gap between OTTO and status lines.
	b.WriteByte('\n')
	b.WriteByte('\n')
	for _, line := range shown {
		if line == "" {
			b.WriteByte('\n')
			continue
		}
		b.WriteString(centerByPlain(line, w))
		b.WriteByte('\n')
	}
	b.WriteString(centerByPlain(cursor, w))
	b.WriteByte('\n')

	// Push the credit to the bottom of the viewport.
	linesSoFar := padTop + artH + 2 + len(shown) + 1
	remaining := h - linesSoFar - 2
	if remaining < 0 {
		remaining = 0
	}
	for i := 0; i < remaining; i++ {
		b.WriteByte('\n')
	}
	b.WriteString(centerByPlain(credit, w))

	return b.String()
}

// centerVisible centers a line whose rendered width is approximately
// visW inside a terminal of totalW. Used for the OTTO art.
func centerVisible(line string, totalW, visW int) string {
	pad := (totalW - visW) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + line
}

// centerByPlain strips ANSI (including OSC 8 hyperlinks) to measure the
// visible width, then left-pads.
func centerByPlain(s string, totalW int) string {
	vis := stripANSIRunes(s)
	pad := (totalW - countRunes(vis)) / 2
	if pad < 0 {
		pad = 0
	}
	return strings.Repeat(" ", pad) + s
}

func stripANSIRunes(s string) string {
	var b strings.Builder
	inEsc := false
	inOSC := false
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		// OSC 8 hyperlinks: ESC ] ... ESC \
		if inOSC {
			if r == 0x1b && i+1 < len(runes) && runes[i+1] == '\\' {
				inOSC = false
				i++
			}
			continue
		}
		if r == 0x1b {
			if i+1 < len(runes) && runes[i+1] == ']' {
				inOSC = true
				i++
				continue
			}
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

func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
