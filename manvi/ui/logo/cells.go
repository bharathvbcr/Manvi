package logo

import "sync"

// The terminal's mark is the published ribbon (svg.go) sampled into character
// cells. Nothing here is drawn by hand: the cells are computed from the same
// legs the SVG fills, so a change to the ribbon's measurements moves both
// renderings, and the test that pins the characters below says so out loud.
//
// A cell is drawn with a quadrant block (▘ ▝ ▖ ▗ and their unions), which
// splits it into four pixels, two across and two down. That is twice the
// horizontal resolution of the half blocks, so the ribbon's slanted edges
// step half as far. The quadrants sit in the same Unicode block (Block
// Elements) as the full block the old tile was drawn with, but they are other
// characters, and nothing here can ask a terminal whether its font has them:
// the only gate is the locale check that already decides whether any rung
// above the name is drawn. They were seen drawn correctly in xterm.js with
// Menlo and nowhere else. The finer sextants were tried and left out because
// that same font showed them as boxes.

// tone is which face of the ribbon a pixel shows.
type tone uint8

const (
	toneNone tone = iota
	// toneLit is a rising leg, toneShade a falling one.
	toneLit
	toneShade
)

// cell is one character cell of the mark.
//
// A terminal cell holds two colours, so a cell holds at most two tones: fg is
// drawn by the glyph and bg, when it is set, fills the rest. bg is only ever
// toneShade, in the cells where the two faces meet; a cell at the ribbon's
// outer edge leaves bg unset, and what is behind its glyph is then whatever
// background the caller's style carries (see ground in logo.go).
//
// at is how far along the ribbon the cell sits, counted in rows of travel from
// the foot of the first leg: up the first leg, down the second, and so on, so
// it runs from 0 to legs x rows - 1. It is what motion.go moves along; a cell
// at rest never reads it. A cell where two legs meet takes the earlier one.
type cell struct {
	r  rune
	fg tone
	bg tone
	at uint8
}

// quadrants maps a four-bit mask of filled pixels to its block character. Bit
// 0 is the upper left pixel, bit 1 the upper right, bit 2 the lower left and
// bit 3 the lower right.
var quadrants = [16]rune{' ', '▘', '▝', '▀', '▖', '▌', '▞', '▛', '▗', '▚', '▐', '▜', '▄', '▙', '▟', '█'}

// The mark's sizes, in cells. A cell is about twice as tall as it is wide, so a
// ribbon 204 units wide and 126 tall wants a little over three columns for
// every row.
const (
	fullCols, fullRows     = 19, 6
	mediumCols, mediumRows = 10, 3
)

var (
	fullCells   = sync.OnceValue(func() [][]cell { return rasterise(fullCols, fullRows) })
	mediumCells = sync.OnceValue(func() [][]cell { return rasterise(mediumCols, mediumRows) })
)

// compactCells is the one-line rung. Four cells cannot be sampled into a
// ribbon, but ▞ and ▚ are block characters that already slant the right way,
// so the legs are written out: rising lit, falling shaded, twice.
var compactCells = []cell{
	{r: '▞', fg: toneLit, at: 0}, {r: '▚', fg: toneShade, at: 1},
	{r: '▞', fg: toneLit, at: 2}, {r: '▚', fg: toneShade, at: 3},
}

// steps is how many values at takes across a set of cells: one per row of
// each leg.
func steps(rows [][]cell) int { return foldLegs * len(rows) }

// samples is how many times each pixel is sampled along each axis.
const samples = 6

// rasterise samples the ribbon into cols x rows cells.
//
// The arithmetic is in whole numbers on purpose. Sample points sit at odd
// multiples of a half step, the ribbon's corners at even ones, and every
// comparison is an integer cross product, so the characters are the same on
// every platform: a float version would be free to fuse a multiply and an add
// on one architecture and not another, and a sample on an edge would then fall
// on different sides of it.
func rasterise(cols, rows int) [][]cell {
	pw, ph := int64(cols*2), int64(rows*2)
	left := int64(foldColumn(0) - foldHalf)
	width := int64(foldColumn(foldLegs)+foldHalf) - left
	height := int64(foldBottom - foldTop)
	// One unit of the ribbon is sx sample steps across and sy down.
	sx, sy := 2*pw*samples, 2*ph*samples

	type leg struct {
		poly [][2]int64
		lit  bool
	}
	legs := make([]leg, foldLegs)
	for i := range foldLegs {
		for _, p := range foldLeg(i) {
			legs[i].poly = append(legs[i].poly, [2]int64{(int64(p.x) - left) * sx, int64(p.y-foldTop) * sy})
		}
		legs[i].lit = foldOnBaseline(i)
	}

	// pixel reports the face a pixel shows and the first leg showing that face
	// in it, or foldLegs when it is empty.
	pixel := func(px, py int64) (tone, int) {
		var lit, shade int
		firstLit, firstShade := foldLegs, foldLegs
		for j := range int64(samples) {
			for i := range int64(samples) {
				x := (2*(px*samples+i) + 1) * width
				y := (2*(py*samples+j) + 1) * height
				for n, l := range legs {
					if !contains(l.poly, x, y) {
						continue
					}
					if l.lit {
						lit++
						firstLit = min(firstLit, n)
					} else {
						shade++
						firstShade = min(firstShade, n)
					}
					break
				}
			}
		}
		switch {
		case (lit+shade)*2 < samples*samples:
			return toneNone, foldLegs
		case lit >= shade:
			return toneLit, firstLit
		default:
			return toneShade, firstShade
		}
	}

	out := make([][]cell, rows)
	for r := range rows {
		out[r] = make([]cell, cols)
		for c := range cols {
			var px, was [4]tone
			var from [4]int
			var lit, shade, empty int
			for k := range 4 {
				px[k], from[k] = pixel(int64(c*2+k%2), int64(r*2+k/2))
				was[k] = px[k]
				switch px[k] {
				case toneLit:
					lit++
				case toneShade:
					shade++
				default:
					empty++
				}
			}
			// Three classes cannot share a cell. The rarer face gives way to
			// the other and the empty pixels stay empty, so the ribbon's edge
			// is kept and neither face's colour is painted outside it.
			if lit > 0 && shade > 0 && empty > 0 {
				from, to := toneShade, toneLit
				if lit < shade {
					from, to = toneLit, toneShade
				}
				for k := range px {
					if px[k] == from {
						px[k] = to
					}
				}
				if from == toneLit {
					lit = 0
				} else {
					shade = 0
				}
			}
			cl := cell{r: ' '}
			switch {
			case lit > 0 && shade > 0:
				cl.fg, cl.bg = toneLit, toneShade
			case lit > 0:
				cl.fg = toneLit
			case shade > 0:
				cl.fg = toneShade
			}
			if cl.fg != toneNone {
				mask := 0
				for k, p := range px {
					if p == cl.fg {
						mask |= 1 << k
					}
				}
				cl.r = quadrants[mask]
				// The cell belongs to the earliest leg it shows. A pixel that
				// gave way to the other face above does not count: it is drawn
				// as part of that face's leg, so it arrives with it.
				leg := foldLegs
				for k := range px {
					if was[k] == cl.fg || (was[k] != toneNone && cl.bg != toneNone) {
						leg = min(leg, from[k])
					}
				}
				// A rising leg is travelled from the bottom row up, a falling
				// one from the top row down.
				along := r
				if legs[leg].lit {
					along = rows - 1 - r
				}
				cl.at = uint8(leg*rows + along)
			}
			out[r][c] = cl
		}
	}
	return out
}

// contains reports whether (x, y) is inside the polygon, by crossing count.
func contains(poly [][2]int64, x, y int64) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		xi, yi := poly[i][0], poly[i][1]
		xj, yj := poly[j][0], poly[j][1]
		if (yi > y) == (yj > y) {
			continue
		}
		// x < xi + (xj-xi)*(y-yi)/(yj-yi), cross-multiplied.
		lhs, rhs := (x-xi)*(yj-yi), (xj-xi)*(y-yi)
		if (yj > yi) == (lhs < rhs) {
			in = !in
		}
	}
	return in
}
