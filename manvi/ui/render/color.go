// Package render is the drawing layer: colours, cells, a screen buffer, and a
// painter that turns two buffers into the smallest escape sequence that gets
// from one to the other.
//
// It knows nothing about the harness. It knows nothing about terminals either,
// beyond the escape vocabulary — it writes to an io.Writer, which is what lets
// every unit of it be tested against a bytes.Buffer instead of a tty.
//
// The one idea worth stating up front: a terminal is a grid of cells, and the
// expensive part of a TUI is not deciding what the grid should contain but
// transmitting it. Redrawing a full screen costs tens of kilobytes per frame,
// which is visible as tearing over ssh and as fan noise locally. So the whole
// layer is built around producing a *diff*.
package render

import (
	"fmt"
	"strconv"
	"strings"
)

// Profile is how much colour the destination terminal can actually show.
//
// This is not a preference. Sending a truecolor SGR to a terminal that does not
// understand it does not degrade — on several common emulators it prints the
// parameters as literal text, which corrupts the frame. So the profile is
// resolved once from the environment and every colour is reduced to it on the
// way out.
type Profile int

const (
	// NoColor emits no SGR colour at all. Attributes still apply, so bold and
	// underline survive in a pipe or under NO_COLOR.
	NoColor Profile = iota
	// ANSI16 is the original eight colours and their bright variants.
	ANSI16
	// ANSI256 is the xterm 256-colour palette.
	ANSI256
	// TrueColor is 24-bit RGB.
	TrueColor
)

// String names the profile for diagnostics.
func (p Profile) String() string {
	switch p {
	case TrueColor:
		return "truecolor"
	case ANSI256:
		return "256"
	case ANSI16:
		return "16"
	}
	return "none"
}

type colorKind uint8

const (
	kindDefault colorKind = iota
	kindANSI
	kind256
	kindRGB
)

// Color is a colour in whichever space it was authored in.
//
// Colours are authored at their full fidelity and reduced at paint time rather
// than at construction. A palette that was pre-reduced to 16 colours could not
// be re-rendered on a better terminal without rebuilding it, and the profile is
// not always known when a theme is defined.
type Color struct {
	kind colorKind
	r    uint8
	g    uint8
	b    uint8
	idx  uint8
}

// Default is the terminal's own foreground or background. It is not black and
// not white: a theme that hardcodes either is unreadable on the other.
var Default = Color{kind: kindDefault}

// ANSI builds one of the sixteen base colours, 0-15.
func ANSI(i uint8) Color { return Color{kind: kindANSI, idx: i & 0x0f} }

// Indexed builds an xterm 256-palette colour.
func Indexed(i uint8) Color { return Color{kind: kind256, idx: i} }

// RGB builds a 24-bit colour.
func RGB(r, g, b uint8) Color { return Color{kind: kindRGB, r: r, g: g, b: b} }

// Hex parses "#rrggbb" or "#rgb". An unparseable string yields Default rather
// than an error: a malformed theme entry should render plainly, not refuse to
// start the UI.
func Hex(s string) Color {
	s = strings.TrimPrefix(strings.TrimSpace(s), "#")
	switch len(s) {
	case 3:
		r, g, b := hexDigit(s[0]), hexDigit(s[1]), hexDigit(s[2])
		if r < 0 || g < 0 || b < 0 {
			return Default
		}
		return RGB(uint8(r*17), uint8(g*17), uint8(b*17))
	case 6:
		v, err := strconv.ParseUint(s, 16, 32)
		if err != nil {
			return Default
		}
		return RGB(uint8(v>>16), uint8(v>>8), uint8(v))
	}
	return Default
}

func hexDigit(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// IsDefault reports whether the colour defers to the terminal.
func (c Color) IsDefault() bool { return c.kind == kindDefault }

// RGBA returns the colour's approximate 8-bit components, which is what
// blending and contrast need. Default has no components and reports false.
func (c Color) RGBA() (r, g, b uint8, ok bool) {
	switch c.kind {
	case kindRGB:
		return c.r, c.g, c.b, true
	case kind256:
		r, g, b := paletteRGB(c.idx)
		return r, g, b, true
	case kindANSI:
		r, g, b := paletteRGB(c.idx)
		return r, g, b, true
	}
	return 0, 0, 0, false
}

// Blend mixes two colours, t running 0 (all c) to 1 (all other). It is how
// gradients and dimmed variants are derived from a theme rather than
// hand-listed, so a re-themed UI stays coherent.
//
// A Default operand has no components to mix, so the other colour is returned
// whole: blending toward "whatever the terminal uses" cannot be approximated.
func (c Color) Blend(other Color, t float64) Color {
	cr, cg, cb, ok1 := c.RGBA()
	or, og, ob, ok2 := other.RGBA()
	switch {
	case !ok1 && !ok2:
		return Default
	case !ok1:
		return other
	case !ok2:
		return c
	}
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	mix := func(a, b uint8) uint8 {
		return uint8(float64(a)*(1-t) + float64(b)*t + 0.5)
	}
	return RGB(mix(cr, or), mix(cg, og), mix(cb, ob))
}

// sgr appends this colour's parameters for the given profile.
//
// fg selects the foreground opcodes (38/39) or the background ones (48/49).
// Reduction happens here, at the last possible moment, so the same Color can
// paint a truecolor terminal and a 16-colour one in the same process.
func (c Color) sgr(b *strings.Builder, p Profile, fg bool) {
	if p == NoColor {
		return
	}
	if c.kind == kindDefault {
		if fg {
			b.WriteString(";39")
		} else {
			b.WriteString(";49")
		}
		return
	}

	col := c
	// Reduce toward what the terminal can show. Each step is lossy and each is
	// preferable to emitting a sequence the terminal will print as text.
	//
	// An RGB colour bound for a 16-colour terminal goes straight there rather
	// than through the 256-colour cube. Routing it through the cube compounds
	// two roundings, and the first one moves the colour in a direction that
	// matters: the cube's six levels per channel lift the two lesser channels
	// of a saturated colour toward its greatest, which is the definition of
	// draining chroma. #f85149 arrives at the cube as #ff5f5f and reaches the
	// sixteen as grey. Reduced once, it stays red.
	switch {
	case col.kind == kindRGB && p < ANSI256:
		col = ANSI(nearest16(col.r, col.g, col.b))
	case col.kind == kindRGB && p < TrueColor:
		col = Indexed(nearest256(col.r, col.g, col.b))
	}
	if col.kind == kind256 && p < ANSI256 {
		r, g, b := paletteRGB(col.idx)
		col = ANSI(nearest16(r, g, b))
	}

	switch col.kind {
	case kindANSI:
		base := 30
		if !fg {
			base = 40
		}
		if col.idx >= 8 {
			base += 60
			b.WriteString(";" + strconv.Itoa(base+int(col.idx)-8))
			return
		}
		b.WriteString(";" + strconv.Itoa(base+int(col.idx)))
	case kind256:
		if fg {
			b.WriteString(";38;5;")
		} else {
			b.WriteString(";48;5;")
		}
		b.WriteString(strconv.Itoa(int(col.idx)))
	case kindRGB:
		if fg {
			b.WriteString(";38;2;")
		} else {
			b.WriteString(";48;2;")
		}
		fmt.Fprintf(b, "%d;%d;%d", col.r, col.g, col.b)
	}
}

// cubeLevels are the six values the 216-colour cube samples each channel at.
// They are not evenly spaced — the gap from 0 to 95 is deliberate in the xterm
// palette — which is why the nearest-colour search is a search and not
// arithmetic.
var cubeLevels = [6]uint8{0, 95, 135, 175, 215, 255}

// ansiRGB is the de-facto xterm rendering of the sixteen base colours. Terminals
// re-theme these freely, so it is an approximation used only for reduction:
// picking which of sixteen slots an arbitrary RGB lands in.
var ansiRGB = [16][3]uint8{
	{0, 0, 0}, {205, 0, 0}, {0, 205, 0}, {205, 205, 0},
	{0, 0, 238}, {205, 0, 205}, {0, 205, 205}, {229, 229, 229},
	{127, 127, 127}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
	{92, 92, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
}

// paletteRGB returns the approximate RGB of a 256-palette index.
func paletteRGB(i uint8) (uint8, uint8, uint8) {
	switch {
	case i < 16:
		c := ansiRGB[i]
		return c[0], c[1], c[2]
	case i < 232:
		n := int(i) - 16
		return cubeLevels[n/36], cubeLevels[(n/6)%6], cubeLevels[n%6]
	default:
		v := uint8(8 + 10*(int(i)-232))
		return v, v, v
	}
}

// nearest256 reduces RGB to the xterm palette.
//
// Both the colour cube and the grey ramp are searched and the closer wins. Using
// the cube alone visibly tints greys — a neutral #808080 lands on a cube entry
// that is not neutral — and the grey ramp is 24 entries of much finer
// resolution than the cube's 6 levels per channel.
func nearest256(r, g, b uint8) uint8 {
	quantize := func(v uint8) int {
		best, bestDist := 0, 1<<31-1
		for i, level := range cubeLevels {
			if d := sq(int(v) - int(level)); d < bestDist {
				best, bestDist = i, d
			}
		}
		return best
	}
	ri, gi, bi := quantize(r), quantize(g), quantize(b)
	cube := uint8(16 + 36*ri + 6*gi + bi)
	cr, cg, cb := paletteRGB(cube)
	cubeDist := dist(r, g, b, cr, cg, cb)

	// Grey ramp: index 232+n has value 8+10n, so the nearest n inverts that.
	n := (int(r) + int(g) + int(b)) / 3
	n = (n - 8 + 5) / 10
	if n < 0 {
		n = 0
	}
	if n > 23 {
		n = 23
	}
	grey := uint8(232 + n)
	gr, gg, gb := paletteRGB(grey)
	if dist(r, g, b, gr, gg, gb) < cubeDist {
		return grey
	}
	return cube
}

// ansiHueSlot maps the six sectors of the hue wheel, in 60-degree steps from
// red, to the base colour that carries that hue.
var ansiHueSlot = [6]uint8{1, 3, 2, 6, 4, 5}

// ansiNeutralSlots are the four base colours with no hue, darkest first.
var ansiNeutralSlots = [4]uint8{0, 8, 7, 15}

// neutralChroma is the most colour a hue can carry and still reduce to grey.
//
// It is a boundary, not a tuning knob: below it a colour is a tinted grey and
// giving it a hue would invent one, above it a colour has a hue and taking it
// away destroys the only signal 16 colours can carry. The palette's own
// colours sit far from it on both sides — its greys reach chroma 19, its
// dimmest status colour 72 — so the line has room either way.
const neutralChroma = 32

// nearest16 reduces RGB to one of the sixteen base colours.
//
// It decomposes rather than searches, and the difference is the whole point.
// The sixteen are not scattered points in RGB: they are six hues at two
// brightnesses plus four greys. A nearest-point search over them measures every
// candidate the same way, and grey sits in the middle of the cube — so a
// mid-tone red is closer to grey than to red by any Euclidean metric, weighted
// or not, and every load-bearing status colour in the palette reduced to bright
// black. Weighting the channels differently moves which colours fall in; it
// cannot move grey out of the middle.
//
// So chroma decides whether the colour has a hue at all, the hue angle decides
// which of the six it is, and only the brightness is chosen by distance. A
// saturated colour cannot reach a grey slot and a grey cannot reach a hue,
// because neither is ever offered the other's candidates.
func nearest16(r, g, b uint8) uint8 {
	hi, lo := r, r
	for _, v := range [2]uint8{g, b} {
		if v > hi {
			hi = v
		}
		if v < lo {
			lo = v
		}
	}

	if int(hi)-int(lo) < neutralChroma {
		return closestByLevel(ansiNeutralSlots[:], (int(r)+int(g)+int(b))/3, meanLevel)
	}

	// Black is offered alongside the hue's two rungs because the sixteen have
	// no dark chromatic slot: the palette's deep crimson is nearer black than
	// it is to a full-strength red, and painting it red would be louder than
	// the colour it stands for.
	base := ansiHueSlot[hueSector(r, g, b, hi, lo)]
	return closestByLevel([]uint8{0, base, base + 8}, int(hi), peakLevel)
}

// hueSector rounds a colour's hue to one of the six the palette carries.
func hueSector(r, g, b, hi, lo uint8) int {
	c := float64(int(hi) - int(lo))
	var h float64
	switch hi {
	case r:
		// g and b are both within chroma of hi, so this lands in [-1,1].
		h = (float64(g) - float64(b)) / c
	case g:
		h = (float64(b)-float64(r))/c + 2
	default:
		h = (float64(r)-float64(g))/c + 4
	}
	if h < 0 {
		h += 6
	}
	return int(h+0.5) % 6
}

// meanLevel is a slot's average channel, which is what a grey is.
func meanLevel(c [3]uint8) int { return (int(c[0]) + int(c[1]) + int(c[2])) / 3 }

// peakLevel is a slot's greatest channel, which is how strongly it states its
// hue. Comparing hues by their mean would rank yellow above red for reasons
// that have nothing to do with brightness.
func peakLevel(c [3]uint8) int {
	m := c[0]
	if c[1] > m {
		m = c[1]
	}
	if c[2] > m {
		m = c[2]
	}
	return int(m)
}

// closestByLevel picks the candidate whose level, by the given measure, is
// nearest the wanted one.
func closestByLevel(candidates []uint8, want int, level func([3]uint8) int) uint8 {
	best, bestDist := candidates[0], 1<<31-1
	for _, idx := range candidates {
		if d := sq(want - level(ansiRGB[idx])); d < bestDist {
			best, bestDist = idx, d
		}
	}
	return best
}

// dist is squared Euclidean distance weighted toward green, used to choose
// among the 256-colour palette's entries.
//
// Weighted because the eye resolves green far better than blue. It is not used
// to reach the sixteen: see nearest16 for why no weighting of a distance can
// do that job.
func dist(r1, g1, b1, r2, g2, b2 uint8) int {
	dr, dg, db := int(r1)-int(r2), int(g1)-int(g2), int(b1)-int(b2)
	return 2*dr*dr + 4*dg*dg + 3*db*db
}

func sq(v int) int { return v * v }
