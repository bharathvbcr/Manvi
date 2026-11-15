// Package logo is the MANVI mark: a ribbon folded four times into an M, as the
// terminal draws it and as the published SVG.
//
// There is one set of measurements (svg.go) and two renderers read it. svg.go
// fills the ribbon's legs as polygons; cells.go samples the same legs into
// character cells, so a harness that lives in a terminal has a mark that
// terminal can draw from block characters alone, at three sizes, without an
// image protocol. The name beside it is ordinary text in the accent
// colour, so it inherits the terminal's own font instead of being spelled out
// in blocks.
//
// The mark used to be a tile with an M knocked out of it on a 7x7 module grid,
// drawn identically by both renderers. That drawing is gone; what survives of
// the idea is that neither rendering is drawn by hand, and that the tests pin
// each to the shared measurements rather than to the other.
//
// Nothing here knows about the theme. Callers pass styles, which is what lets
// the TUI draw the mark in its accent, the CLI draw it against a pipe's colour
// profile, and the tests draw it into a plain buffer.
package logo

import (
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// Name is the wordmark, set in the terminal's own font.
const Name = "manvi"

// Colors are the styles a caller lends the mark. Roles, not colours, so the
// theme stays the one place a colour is chosen.
type Colors struct {
	// Lit paints the ribbon's rising legs and Shade its falling ones. Where the
	// two meet inside one cell the cell is drawn in Lit's foreground on Shade's
	// foreground, so only their foregrounds decide what the fold looks like.
	Lit   render.Style
	Shade render.Style
	// Word paints the name, Tag the line under it.
	Word render.Style
	Tag  render.Style
	// Glint is the colour a face is lifted toward while light crosses it (see
	// Motion). Left at its zero value, which is the terminal's default colour,
	// the mark has no glint: it still unfolds, but nothing recolours it. A
	// caller on a terminal with a single red lends none for exactly that
	// reason, since a lifted red there is the same red.
	Glint render.Color
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
	// SizeCompact is one line: the ribbon as four slanted blocks, and the name.
	SizeCompact
	// SizeMedium is the ribbon three rows tall, with the name beside it.
	SizeMedium
	// SizeFull is the ribbon six rows tall, the size at which its folds have
	// room to read as folds.
	SizeFull
)

// gap separates the ribbon from the name on the tall rungs; the one-line rung
// uses a single space, as any word after a glyph would.
const gap = 2

// cells returns the ribbon for a rung, or nil for the rungs that have none.
func (s Size) cells() [][]cell {
	switch s {
	case SizeFull:
		return fullCells()
	case SizeMedium:
		return mediumCells()
	case SizeCompact:
		return [][]cell{compactCells}
	}
	return nil
}

// Width returns the cells a size needs, tagline excluded.
func (s Size) Width() int {
	switch s {
	case SizeFull:
		return fullCols + gap + len(Name)
	case SizeMedium:
		return mediumCols + gap + len(Name)
	case SizeCompact:
		return len(compactCells) + 1 + len(Name)
	case SizeText:
		return len(Name)
	}
	return 0
}

// Height returns the rows a size needs, tagline excluded.
func (s Size) Height() int {
	switch s {
	case SizeFull:
		return fullRows
	case SizeMedium:
		return mediumRows
	case SizeCompact, SizeText:
		return 1
	}
	return 0
}

// Fit picks the largest size that fits in w cells and h rows.
//
// unicode gates the ribbon rather than the whole logo. Without block elements
// it would have to be drawn in slashes, which reads as noise rather than as a
// shape, so those rungs are skipped and the name is drawn on its own.
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
// returns the rows it used. A tagline is drawn under the name on the tall
// rungs when there is room for it.
//
// The lockup is centred once, by its widest row, and every row starts in that
// column. Centring each row by its own width is what this used to do, and it
// sheared the mark: the row carrying the name and the row carrying the tagline
// are wider than the rest, so they started further left than their neighbours.
//
// It returns 0 without drawing when nothing fits, so callers can treat that as
// "no splash" rather than having to measure first.
func Draw(b *render.Buffer, r render.Rect, c Colors, unicode bool, tagline string) int {
	return DrawAt(b, r, c, unicode, tagline, Motion{})
}

// DrawAt is Draw for the mark in motion. It paints the same cells at every
// tick and returns the same row count, so a caller can lay out around the mark
// once and animate it without anything else moving.
func DrawAt(b *render.Buffer, r render.Rect, c Colors, unicode bool, tagline string, m Motion) int {
	if b == nil || r.Empty() {
		return 0
	}
	size := Fit(r.W, r.H, unicode)
	if size == SizeNone {
		return 0
	}
	lines := LinesAt(size, c, unicode, tagline, r.W, m)
	widest := 0
	for _, l := range lines {
		widest = max(widest, l.Width())
	}
	x := r.X + max(0, (r.W-widest)/2)
	for i, l := range lines {
		if i >= r.H {
			return i
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
	return LinesAt(size, c, unicode, tagline, maxWidth, Motion{})
}

// LinesAt is Lines for the mark in motion. Only the ribbon moves: the name and
// the tagline are written whole from the first frame, because they are what
// fixes each line's width and so where the lockup is centred.
func LinesAt(size Size, c Colors, unicode bool, tagline string, maxWidth int, m Motion) []render.Line {
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
		l := Ribbon(c, m).Append(" "+Name, c.Word)
		if tagline != "" && l.Width()+2+render.StringWidth(tagline) <= maxWidth {
			l = l.Append("  "+tagline, c.Tag)
		}
		return []render.Line{l.Truncate(maxWidth)}
	}

	// The name sits on the ribbon's middle row (the upper of the two when the
	// row count is even), and the tagline directly under it, so the lockup has
	// one optical centre rather than two stacked blocks.
	rows := size.cells()
	nameRow := (len(rows) - 1) / 2
	tagRow := nameRow + 1
	lines := make([]render.Line, len(rows))
	for y, row := range rows {
		l := cellLine(row, c, m, steps(rows))
		switch {
		case y == nameRow:
			l = l.Append(spaces(gap), ground(c)).Append(Name, c.Word)
		case y == tagRow && tagline != "" && len(row)+gap+render.StringWidth(tagline) <= maxWidth:
			l = l.Append(spaces(gap), ground(c)).Append(tagline, c.Tag)
		}
		lines[y] = l
	}
	return lines
}

// Ribbon is the one-line ribbon on its own, without the name: four cells, for
// a caller that wants the mark as a glyph inside a line of its own. It is the
// same four cells the one-line rung of Lines starts with.
func Ribbon(c Colors, m Motion) render.Line {
	return cellLine(compactCells, c, m, foldLegs)
}

// ground is the style of a cell the mark leaves empty: a space on the
// background the caller lent the ribbon.
//
// An empty cell is still written, because a line is drawn left to right and
// the ribbon's rows do not start in their first column. Written without a
// background it would take the terminal's own, and in a theme that paints its
// background that is a hole the shape of the mark's bounding box. So it takes
// the ribbon's background, which is the theme's in the TUI and nothing at all
// where the caller lends none (a pipe, a test).
func ground(c Colors) render.Style {
	return render.Style{Bg: c.Lit.Bg}
}

// cellLine renders one row of the ribbon, n steps long, as it stands at m.
//
// Neighbouring cells drawn in the same style are written as one run, so a row
// at rest is a handful of spans rather than one per cell.
func cellLine(row []cell, c Colors, m Motion, n int) render.Line {
	var l render.Line
	shown := m.shown(n)
	var text []rune
	var style render.Style
	flush := func() {
		if len(text) > 0 {
			l = l.Append(string(text), style)
			text = text[:0]
		}
	}
	for _, cl := range row {
		// A cell the unfold has not reached is written as the mark's own
		// background, exactly as a cell the ribbon never covers is.
		if cl.fg != toneNone && int(cl.at) >= shown {
			cl = cell{r: ' '}
		}
		level, tail := m.glow(int(cl.at), n)
		s := cellStyle(cl, c, level, tail)
		if s != style {
			flush()
			style = s
		}
		text = append(text, cl.r)
	}
	flush()
	return l
}

// cellStyle is the style one cell is drawn in, with light on it at level out
// of tail.
func cellStyle(cl cell, c Colors, level, tail int) render.Style {
	face := func(t tone) render.Style {
		s := c.Lit
		if t == toneShade {
			s = c.Shade
		}
		s.Fg = lift(s.Fg, c.Glint, level, tail, t == toneShade)
		return s
	}
	switch {
	case cl.fg == toneNone:
		return ground(c)
	case cl.bg == toneNone:
		return face(cl.fg)
	}
	// Both faces in one cell: the glyph is one and the cell's background is
	// the other, which is the only place the mark's own colours reach a
	// background.
	s := face(cl.fg)
	s.Bg = face(cl.bg).Fg
	return s
}

func spaces(n int) string {
	out := make([]byte, n)
	for i := range out {
		out[i] = ' '
	}
	return string(out)
}
