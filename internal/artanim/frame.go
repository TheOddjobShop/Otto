// Package artanim renders Otto's animated terminal art: a spinning 3D
// lightbulb, an audio-reactive bar cluster, and the boot reveal.
//
// The lightbulb is a real rasterizer rather than ASCII frames — it plots a
// parametric bulb, filament and Edison screw into a Z-buffered character grid
// with two-light shading, then flattens to 24-bit ANSI. That is why it can spin
// smoothly at any size and scale: there is nothing to redraw by hand.
package artanim

import (
	"fmt"
	"math"
	"strings"
)

// Shades go from empty to full. First entry is background (skipped).
var shades = []rune(" ░▒▓█")

// Frame is a 2D grid of characters + color indices + Z-buffer.
type Frame struct {
	W, H   int
	chars  []rune
	colors []int
	zbuf   []float64
}

// NewFrame allocates a frame with the given size and reset buffers.
func NewFrame(w, h int) *Frame {
	n := w * h
	f := &Frame{W: w, H: h, chars: make([]rune, n), colors: make([]int, n), zbuf: make([]float64, n)}
	for i := range f.chars {
		f.chars[i] = ' '
	}
	for i := range f.colors {
		f.colors[i] = -1
	}
	for i := range f.zbuf {
		f.zbuf[i] = -1e9
	}
	return f
}

// Plot places a shaded character at screen-projected position with the
// given component color. Matches the JS `plot` function.
func (f *Frame) Plot(px, py, pz, nx, ny, nz float64, comp int, scaleX, scaleY, centerX, centerY float64, key, fill [3]float64) {
	d1 := math.Max(0, nx*key[0]+ny*key[1]+nz*key[2])
	d2 := math.Max(0, nx*fill[0]+ny*fill[1]+nz*fill[2])
	b := 0.35 + d1*0.55 + d2*0.30
	if b > 1 {
		b = 1
	}
	idx := int(math.Floor(b*float64(len(shades)-1))) + 1
	if idx > len(shades)-1 {
		idx = len(shades) - 1
	}
	ch := shades[idx]

	sx := int(math.Round(px*scaleX + centerX))
	sy := int(math.Round(-py*scaleY + centerY))
	if sx < 0 || sx >= f.W || sy < 0 || sy >= f.H {
		return
	}
	k := sy*f.W + sx
	if pz > f.zbuf[k] {
		f.zbuf[k] = pz
		f.chars[k] = ch
		f.colors[k] = comp
	}
}

// RenderANSI flattens the frame to an ANSI-colored string using a
// palette of 24-bit RGB hex colors like "#ff0000". Works in any
// truecolor terminal (iTerm2, modern Terminal.app, Alacritty, etc).
func (f *Frame) RenderANSI(palette []string) string {
	codes := make([]string, len(palette))
	for i, hex := range palette {
		codes[i] = ansi24(hex)
	}
	var b strings.Builder
	for y := 0; y < f.H; y++ {
		cur := -1
		for x := 0; x < f.W; x++ {
			k := y*f.W + x
			ch := f.chars[k]
			col := f.colors[k]
			if ch == ' ' || col == -1 {
				if cur != -1 {
					b.WriteString("\x1b[0m")
					cur = -1
				}
				b.WriteRune(' ')
				continue
			}
			if col != cur {
				if cur != -1 {
					b.WriteString("\x1b[0m")
				}
				b.WriteString(codes[col])
				cur = col
			}
			b.WriteRune(ch)
		}
		if cur != -1 {
			b.WriteString("\x1b[0m")
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Light rig (two key lights + ambient), matched to the JS originals.
func defaultLights() (key, fill [3]float64) {
	key = norm([3]float64{-0.45, 0.82, 0.40})
	fill = norm([3]float64{0.40, -0.30, 0.55})
	return
}

func norm(v [3]float64) [3]float64 {
	m := math.Hypot(math.Hypot(v[0], v[1]), v[2])
	return [3]float64{v[0] / m, v[1] / m, v[2] / m}
}

// ansi24 converts "#rrggbb" to an ANSI 24-bit foreground escape.
func ansi24(hex string) string {
	if len(hex) < 7 || hex[0] != '#' {
		return ""
	}
	var r, g, bl int
	fmt.Sscanf(hex[1:], "%02x%02x%02x", &r, &g, &bl)
	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", r, g, bl)
}
