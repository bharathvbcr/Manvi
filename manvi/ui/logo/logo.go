// Package logo is the MANVI mark, drawn from one module grid.
//
// The mark is a filled tile with an M knocked out of it, on a 7x7 module grid,
// and that is the whole design. It is deliberately a shape rather than a
// rendered wordmark: a harness that lives in a terminal should have a mark a
// terminal can draw exactly, at any size, without approximating a curve. The
// name beside it is ordinary text in the accent colour, so it inherits the
// terminal's own font instead of being spelled out in blocks.
//
// svg.go emits the same grid for use outside a terminal, so the published asset
// and the splash screen cannot drift — there is one bitmap, and both renderers
// read it.
//
// Nothing here knows about the theme. Callers pass styles, which is what lets
// the TUI draw the mark in its accent, the CLI draw it against a pipe's colour
// profile, and the tests draw it into a plain buffer.
package logo

import (
	"manvi/ui/render"
)

// The grid. '#' is the knockout — the M — and '.' is the tile's field. One
// module of field surrounds the letter on every side, which is what makes the
// shape read as a stamped tile rather than a letter on a background.
const (
	markRows = 7
	markCols = 7
)

var mark = []string{
	".......",
	".#...#.",
	".##.##.",
	".#.#.#.",
	".#...#.",
	".#...#.",
	".......",
}

// Name is the wordmark, set in the terminal's own font.
const Name = "manvi"

// Colors are the styles a caller lends the mark. Roles, not colours, so the
// theme stays the one place a colour is chosen.
type Colors struct {
	// Tile paints the field, Glyph the M knocked out of it.
	Tile  render.Style
	Glyph render.Style
	// Word paints the name, Tag the line under it.
	Word render.Style
	Tag  render.Style
}

// Size is how much of the mark fits.
//
// The fallbacks are a ladder rather than a switch between "logo" and "nothing":
// a narrow pane still gets a mark, an ASCII-only terminal still gets the name,
// and the one thing that never happens is a logo drawn wider than the space it
// was given. A splash screen that corrupts the frame is worse than no splash
// screen.
type Size int

// The rungs, widest first.
const (
	// SizeNone means not even the name fits.
	SizeNone Size = iota
	// SizeText is the name alone.
	SizeText
	// SizeCompact is one line: a single filled cell and the name.
	SizeCompact
	// SizeMedium is the tile at one cell per module, with the name beside it.
	SizeMedium
	// SizeFull is the tile at two cells per module, which is roughly square
	// once a terminal's 1:2 cell aspect is accounted for.
	SizeFull
)

// gap separates the tile from the name.
const gap = 2

// Width returns the cells a size needs, tagline excluded.
func (s Size) Width() int {
	switch s {
	case SizeFull:
		return markCols*2 + gap + len(Name)
	case SizeMedium:
		return markCols + gap + len(Name)
	case SizeCompact:
		return 2 + len(Name)
	case SizeText:
		return len(Name)
	}
	return 0
}

// Height returns the rows a size needs, tagline excluded.
func (s Size) Height() int {
	switch s {
	case SizeFull, SizeMedium:
		return markRows
	case SizeCompact, SizeText:
		return 1
	}
	return 0
}

// Fit picks the largest size that fits in w cells and h rows.
//
// unicode gates the tile sizes rather than the whole logo. Without block
// elements the tile would have to be a field of '#', which reads as noise
// rather than as a shape, so those rungs are skipped and the name is drawn on
// its own.
func Fit(w, h int, unicode bool) Size {
	for _, s := range []Size{SizeFull, SizeMedium, SizeCompact, SizeText} {
		if !unicode && s > SizeText {
			continue
		}
		if w >= s.Width() && h >= s.Height() {
			return s
		}
	}
	return SizeNone
}

// Draw paints the largest logo that fits in r, centred horizontally, and
// returns the rows it used. A tagline is drawn under the tile sizes when there
// is a spare row for it.
//
// It returns 0 without drawing when nothing fits, so callers can treat that as
// "no splash" rather than having to measure first.
func Draw(b *render.Buffer, r render.Rect, c Colors, unicode bool, tagline string) int {
	if b == nil || r.Empty() {
		return 0
	}
	size := Fit(r.W, r.H, unicode)
	if size == SizeNone {
		return 0
	}
	lines := Lines(size, c, unicode, tagline, r.W)
	for i, l := range lines {
		if i >= r.H {
			return i
		}
		x := r.X + (r.W-l.Width())/2
		if x < r.X {
			x = r.X
		}
		l.Truncate(r.W).Draw(b, x, r.Y+i)
	}
	return len(lines)
}

// Lines renders the logo as styled lines, which is what the CLI writes to
// stdout and what Draw paints into a buffer. One renderer, two destinations.
//
// maxWidth bounds the tagline; the mark's own width is fixed by the size.
func Lines(size Size, c Colors, unicode bool, tagline string, maxWidth int) []render.Line {
	// A caller that names a size wider than the width it also passes gets the
	// largest rung that fits, not an overflowing one. Draw always agrees with
	// itself because it asks Fit first; the callers that pick a rung directly —
	// the transcript's session line, the CLI — are the ones this protects, and
	// they are exactly the callers whose width comes from a pane that can be
	// dragged narrower.
	if size != SizeNone && size.Width() > maxWidth {
		size = Fit(maxWidth, size.Height(), unicode)
	}
	switch size {
	case SizeNone:
		return nil

	case SizeText:
		return []render.Line{render.Styled(Name, c.Word).Truncate(maxWidth)}

	case SizeCompact:
		l := render.Styled("█", c.Tile).Append(" "+Name, c.Word)
		if tagline != "" && l.Width()+2+render.StringWidth(tagline) <= maxWidth {
			l = l.Append("  "+tagline, c.Tag)
		}
		return []render.Line{l.Truncate(maxWidth)}
	}

	scale := 1
	if size == SizeFull {
		scale = 2
	}

	// The name sits on the tile's middle row, and the tagline directly under
	// it, so the lockup has one optical centre rather than two stacked blocks.
	nameRow := markRows / 2
	tagRow := nameRow + 1
	lines := make([]render.Line, markRows)
	for y := range markRows {
		l := gridLine(mark[y], scale, c.Tile, c.Glyph)
		switch {
		case y == nameRow:
			l = l.Append(spaces(gap), render.Style{}).Append(Name, c.Word)
		case y == tagRow && tagline != "" && markCols*scale+gap+render.StringWidth(tagline) <= maxWidth:
			l = l.Append(spaces(gap), render.Style{}).Append(tagline, c.Tag)
		}
		lines[y] = l
	}
	return lines
}

// gridLine renders one row of the tile.
//
// Blank modules are never painted — the grid has none today, but a mark that
// stamped a rectangle of background over whatever is behind it would be a
// splash that cannot be drawn over a transcript.
func gridLine(row string, scale int, field, on render.Style) render.Line {
	var l render.Line
	for _, m := range row {
		switch m {
		case '#':
			l = l.Append(repeat("█", scale), on)
		case '.':
			l = l.Append(repeat("█", scale), field)
		default:
			l = l.Append(spaces(scale), render.Style{})
		}
	}
	return l
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for range n {
		out = append(out, s...)
	}
	return string(out)
}

func spaces(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}
