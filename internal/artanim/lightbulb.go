package artanim

import "math"

// The lightbulb: a glass envelope with a visible filament, an Edison screw
// base with a helical thread, and a contact tip. Component indexes match the
// palette below.
const (
	lbGlass    = 0
	lbFilament = 1
	lbBaseHigh = 2
	lbBaseMid  = 3
	lbBaseLow  = 4
	lbTip      = 5
)

var lightbulbPalette = []string{
	"#fef3c7", // glass       – warm pale cream
	"#f59e0b", // filament    – glowing amber
	"#e3b778", // base_high   – bright brass crest
	"#9c7a4e", // base_mid    – mid brass flank
	"#4a3418", // base_low    – deep shadow valley
	"#4b5563", // tip         – dark metal
}

// RenderLightbulb renders a single frame at the given spin angle.
// Returns an ANSI-colored string at the default (splash) size.
func RenderLightbulb(w, h int, angle float64) string {
	return RenderLightbulbScaled(w, h, angle, 1.0)
}

// RenderLightbulbScaled renders at a given scale factor (1.0 = splash,
// 0.5 = compact header).
func RenderLightbulbScaled(w, h int, angle, scale float64) string {
	const tiltX = 0.32
	const tiltZ = 0.22
	key, fill := defaultLights()

	scaleX := 16.0 * scale
	scaleY := 8.0 * scale
	centerX := float64(w) / 2.0
	centerY := float64(h)/2.0 + 1.0

	f := NewFrame(w, h)

	// transform takes a point in object space and rotates it.
	transform := func(x, y, z float64) (float64, float64, float64) {
		cx, sx := math.Cos(tiltX), math.Sin(tiltX)
		y1 := y*cx - z*sx
		z1 := y*sx + z*cx

		cz, sz := math.Cos(tiltZ), math.Sin(tiltZ)
		x2 := x*cz - y1*sz
		y2 := x*sz + y1*cz

		ca, sa := math.Cos(angle), math.Sin(angle)
		x3 := x2*ca + z1*sa
		z3 := -x2*sa + z1*ca

		return x3, y2, z3
	}

	plot := func(x, y, z, nx, ny, nz float64, comp int) {
		px, py, pz := transform(x, y, z)
		tnx, tny, tnz := transform(nx, ny, nz)
		f.Plot(px, py, pz, tnx, tny, tnz, comp, scaleX, scaleY, centerX, centerY, key, fill)
	}

	// Glass bulb: only back-facing drawn so filament shows
	bulbCy := 0.40
	bulbR := 0.60
	bulbBot := -0.12
	bulbTop := 1.00
	for y := bulbBot; y <= bulbTop; y += 0.025 {
		dy := y - bulbCy
		r := math.Sqrt(math.Max(0, bulbR*bulbR-dy*dy))
		for t := 0.0; t < math.Pi*2; t += 0.035 {
			ct, st := math.Cos(t), math.Sin(t)
			x := r * ct
			z := r * st
			nx := x / bulbR
			ny := dy / bulbR
			nz := z / bulbR
			// skip front-facing half
			_, _, nvz := transform(nx, ny, nz)
			if nvz > 0 {
				continue
			}
			plot(x, y, z, nx, ny, nz, lbGlass)
		}
	}

	// Filament: two vertical supports + drooping arc
	filTop := 0.30
	filSpan := 0.16
	filDip := 0.09
	for y := bulbBot + 0.02; y <= filTop; y += 0.028 {
		for a := 0.0; a < math.Pi*2; a += 0.9 {
			ca, sa := math.Cos(a), math.Sin(a)
			plot(-filSpan+0.013*ca, y, 0.013*sa, ca, 0, sa, lbFilament)
			plot(filSpan+0.013*ca, y, 0.013*sa, ca, 0, sa, lbFilament)
		}
	}
	for t := -1.0; t <= 1; t += 0.02 {
		x := t * filSpan
		y := filTop - filDip*(1-t*t)
		for a := 0.0; a < math.Pi*2; a += 0.9 {
			ca, sa := math.Cos(a), math.Sin(a)
			plot(x, y+0.013*ca, 0.013*sa, 0, ca, sa, lbFilament)
		}
	}

	// Base (Edison screw with jagged helical thread)
	baseYtop := bulbBot
	baseYbot := -0.80
	baseR := 0.30
	threadAmp := 0.055
	threadPitch := 0.12
	for y := baseYbot; y <= baseYtop; y += 0.020 {
		for t := 0.0; t < math.Pi*2; t += 0.040 {
			var r float64
			var comp int
			if y > baseYtop-0.05 {
				r = baseR + 0.010
				comp = lbBaseHigh
			} else {
				phase := (2 * math.Pi * y / threadPitch) - t
				p := math.Mod(math.Mod(phase, math.Pi*2)+math.Pi*2, math.Pi*2)
				u := p / (math.Pi * 2)
				var tri float64
				if u < 0.5 {
					tri = u*4 - 1
				} else {
					tri = 3 - u*4
				}
				r = baseR + threadAmp*tri
				switch {
				case tri > 0.4:
					comp = lbBaseHigh
				case tri > -0.4:
					comp = lbBaseMid
				default:
					comp = lbBaseLow
				}
			}
			ct, st := math.Cos(t), math.Sin(t)
			plot(r*ct, y, r*st, ct, 0, st, comp)
		}
	}
	for t := 0.0; t < math.Pi*2; t += 0.08 {
		ct, st := math.Cos(t), math.Sin(t)
		for r := 0.12; r <= baseR; r += 0.035 {
			plot(r*ct, baseYbot, r*st, 0, -1, 0, lbBaseMid)
		}
	}

	// Tip (small contact hemisphere)
	tipR := 0.12
	tipCy := baseYbot
	for phi := 0.0; phi <= math.Pi/2; phi += 0.12 {
		cp, sp := math.Cos(phi), math.Sin(phi)
		y := tipCy - tipR*sp
		r := tipR * cp
		for t := 0.0; t < math.Pi*2; t += 0.15 {
			ct, st := math.Cos(t), math.Sin(t)
			plot(r*ct, y, r*st, cp*ct, -sp, cp*st, lbTip)
		}
	}

	return f.RenderANSI(lightbulbPalette)
}
