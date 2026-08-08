package artanim

import (
	"math"
	"strings"
)

// Block characters for vertical bar tips, smallest to tallest.
var waveChars = []rune(" ▁▂▃▄▅▆▇█")

// RenderSiriBars draws a mini Siri-style spectrum: a small cluster of
// equal-width vertical bars, spaced apart, centered horizontally,
// with independent heights that oscillate around a center-heavy
// envelope. amplitude scales the overall motion (0 = still, 1 = full
// height); t is a monotonically-increasing time driver in seconds-ish.
// maxH is how many rows tall the bars can go.
func RenderSiriBars(w, maxH int, amplitude, t float64) string {
	if w < 10 || maxH < 2 {
		return ""
	}
	if amplitude < 0 {
		amplitude = 0
	}
	if amplitude > 1 {
		amplitude = 1
	}

	numBars := 7
	gap := 2 // spaces between bars
	barCell := 1
	totalW := numBars*barCell + (numBars-1)*gap
	leftPad := (w - totalW) / 2
	if leftPad < 0 {
		leftPad = 0
	}

	// Per-bar heights, normalized [0..maxH]. Each bar has its own phase
	// offset so they don't march in lockstep. A center-heavy envelope
	// (bigger at middle, smaller at ends) gives the Siri "bubble" look.
	heights := make([]float64, numBars)
	for i := range heights {
		phase := t*2.1 + float64(i)*0.7
		// Distance from center in [0, 1]
		dist := math.Abs(float64(i)-float64(numBars-1)/2.0) / (float64(numBars-1) / 2.0)
		envelope := 0.55 + 0.45*(1.0-dist*dist) // parabolic
		// Baseline sine; scale by amplitude and envelope.
		s := 0.5 + 0.5*math.Sin(phase)
		heights[i] = amplitude * envelope * s * float64(maxH)
	}

	// Render row-by-row, top → bottom.
	color := ansi24("#f59e0b") // amber (matches lightbulb filament)
	dim := ansi24("#78350f")   // deeper amber for idle
	reset := "\x1b[0m"

	var b strings.Builder
	for row := maxH - 1; row >= 0; row-- {
		b.WriteString(strings.Repeat(" ", leftPad))
		for i, h := range heights {
			rowLevel := float64(row)
			diff := h - rowLevel
			var ch rune
			var col string
			if diff >= 1 {
				ch = '█'
				col = color
			} else if diff > 0 {
				// Partial block at the top of the bar.
				idx := int(math.Round(diff * float64(len(waveChars)-1)))
				if idx < 1 {
					idx = 1
				}
				if idx > len(waveChars)-1 {
					idx = len(waveChars) - 1
				}
				ch = waveChars[idx]
				col = color
			} else {
				// Empty cell; render a dim "stem dot" on the bottom row
				// so the cluster is visible even at idle.
				if row == 0 && amplitude < 0.15 {
					ch = '▁'
					col = dim
				} else {
					ch = ' '
					col = ""
				}
			}
			if col != "" {
				b.WriteString(col)
				b.WriteRune(ch)
				b.WriteString(reset)
			} else {
				b.WriteRune(ch)
			}
			if i < numBars-1 {
				b.WriteString(strings.Repeat(" ", gap))
			}
		}
		if row > 0 {
			b.WriteByte('\n')
		}
	}
	return b.String()
}
