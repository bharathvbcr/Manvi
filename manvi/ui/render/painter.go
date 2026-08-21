package render

import (
	"bytes"
	"io"
	"strconv"
)

// Cursor is where the caret should sit after a frame, and whether it shows.
type Cursor struct {
	X, Y    int
	Visible bool
}

// Painter turns successive buffers into the escape sequences that get a
// terminal from one to the next.
//
// It owns two buffers: the one the UI draws into, and the one the terminal is
// believed to be showing. Flush compares them, writes the difference, and swaps.
//
// The frame is assembled into memory and written with a single Write. Writing
// incrementally is the standard way to get a TUI that tears: a terminal renders
// whatever has arrived when it next paints, so a frame split across several
// writes can be shown half-updated, and over ssh the halves can be seconds
// apart.
type Painter struct {
	profile Profile
	cur     *Buffer
	prev    *Buffer
	scratch bytes.Buffer

	// full forces the next flush to repaint every cell. Set on construction, on
	// resize, and by Invalidate — the three cases where what the terminal is
	// showing is no longer known.
	full bool

	// style is the SGR state the terminal is believed to be in. Tracking it is
	// what keeps a styled frame from costing an SGR sequence per cell.
	style    Style
	styleSet bool

	// lastCursor is where the caret was left. Together with "no cell changed"
	// it is what lets an idle frame write nothing at all.
	lastCursor Cursor
	// painted records that at least one frame has been written, so the first
	// call cannot be mistaken for an idle one.
	painted bool
}

// NewPainter builds a painter for a screen of the given size.
func NewPainter(w, h int, p Profile) *Painter {
	return &Painter{
		profile: p,
		cur:     NewBuffer(w, h),
		prev:    NewBuffer(w, h),
		full:    true,
	}
}

// Buffer is the frame under construction. The UI draws here.
func (p *Painter) Buffer() *Buffer { return p.cur }

// Profile reports the colour profile in use.
func (p *Painter) Profile() Profile { return p.profile }

// SetProfile changes the colour profile and forces a repaint.
func (p *Painter) SetProfile(prof Profile) {
	p.profile = prof
	p.Invalidate()
}

// Size reports the current dimensions.
func (p *Painter) Size() (int, int) { return p.cur.W, p.cur.H }

// Resize reshapes both buffers and forces a full repaint.
func (p *Painter) Resize(w, h int) {
	if w == p.cur.W && h == p.cur.H {
		return
	}
	p.cur.Resize(w, h)
	p.prev.Resize(w, h)
	p.full = true
}

// Invalidate discards the painter's belief about the screen.
//
// Needed whenever something else wrote to the terminal: a Ctrl+L, a resume from
// suspend, a subprocess that took the tty. Without it the diff would skip cells
// that are correct in the model and wrong on screen, and the corruption would
// persist for as long as those cells do not change.
func (p *Painter) Invalidate() {
	p.full = true
	p.styleSet = false
}

// Flush writes the difference to out and adopts the new frame as current.
func (p *Painter) Flush(out io.Writer, cur Cursor) error {
	p.scratch.Reset()
	b := &p.scratch

	// The cursor is hidden for the duration of the paint. Without this it
	// visibly races across the screen following the writes, which on a frame
	// with many small changes looks like flicker.
	b.WriteString("\x1b[?25l")
	prefix := b.Len()
	p.styleSet = false

	// Position is tracked so a run that continues where the last one ended
	// needs no cursor move. On text-heavy frames most runs do.
	lastX, lastY := -1, -1

	for y := 0; y < p.cur.H; y++ {
		x := 0
		for x < p.cur.W {
			if !p.full && p.same(x, y) {
				x++
				continue
			}
			start := x
			// A continuation cell cannot be addressed on its own — the glyph
			// belongs to the cell on its left, and repainting from here would
			// draw the right half of a character the terminal has no way to
			// render.
			if p.cur.Cell(start, y).IsContinuation() && start > 0 {
				start--
			}

			end := p.runEnd(start, y)

			if p.eraseRest(b, start, y, &lastX, &lastY) {
				x = p.cur.W
				continue
			}

			if lastY != y || lastX != start {
				b.WriteString("\x1b[")
				b.WriteString(strconv.Itoa(y + 1))
				b.WriteByte(';')
				b.WriteString(strconv.Itoa(start + 1))
				b.WriteByte('H')
			}

			col := start
			for col <= end && col < p.cur.W {
				cell := p.cur.Cell(col, y)
				if cell.IsContinuation() {
					// Already emitted as the second half of the wide rune to
					// the left; the terminal's cursor has moved over it.
					col++
					continue
				}
				p.applyStyle(b, cell.Style)
				if cell.R == 0 {
					b.WriteByte(' ')
				} else {
					b.WriteRune(cell.R)
				}
				b.WriteString(cell.Comb)
				p.prev.SetCell(col, y, cell)
				w := RuneWidth(cell.R)
				if w == 2 && col+1 < p.cur.W {
					p.prev.SetCell(col+1, y, p.cur.Cell(col+1, y))
				}
				if w < 1 {
					w = 1
				}
				col += w
			}
			lastX, lastY = col, y
			x = col
		}
	}

	// The style is reset at the end of every frame. Leaving the terminal in a
	// coloured state means anything else that writes — a panic trace, a shell
	// prompt after a crash — inherits it.
	if p.styleSet {
		b.WriteString("\x1b[0m")
		p.styleSet = false
	}
	// Measured before the cursor block, which always writes when the caret is
	// visible and would otherwise mask an idle frame.
	changed := b.Len() > prefix

	if cur.Visible {
		b.WriteString("\x1b[")
		b.WriteString(strconv.Itoa(cur.Y + 1))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(cur.X + 1))
		b.WriteByte('H')
		b.WriteString("\x1b[?25h")
	}

	// An idle frame writes nothing. Emitting even the hide/show pair every tick
	// would put a steady trickle on the wire — which over ssh is the difference
	// between a session that is quiet when nothing happens and one that is not,
	// and which makes the diffing above pointless.
	if p.painted && !p.full && !changed && cur == p.lastCursor {
		return nil
	}

	p.full = false
	p.painted = true
	p.lastCursor = cur
	_, err := out.Write(b.Bytes())
	return err
}

// same reports whether a cell is already correct on screen.
func (p *Painter) same(x, y int) bool {
	a, b := p.cur.Cell(x, y), p.prev.Cell(x, y)
	return a.R == b.R && a.Comb == b.Comb && a.Style.Equal(b.Style)
}

// runEnd finds the last column worth including in one run starting at start.
//
// Runs absorb short stretches of unchanged cells rather than stopping at the
// first one. A cursor move costs six to ten bytes, so breaking a run to skip
// three identical cells sends more data than repainting them — and each break
// is another chance for the terminal to paint a partial line.
func (p *Painter) runEnd(start, y int) int {
	const gapTolerance = 6
	last := start
	gap := 0
	for x := start; x < p.cur.W; x++ {
		if p.full || !p.same(x, y) {
			last = x
			gap = 0
			continue
		}
		gap++
		if gap > gapTolerance {
			break
		}
	}
	return last
}

// eraseRest emits an erase-to-end-of-line when the remainder of the row is
// uniform blanks, and reports whether it did.
//
// This is the difference between a cheap frame and an expensive one on a screen
// with wide empty regions — a scrollback pane on a 200-column terminal is
// mostly trailing blanks, and EL is three bytes against two hundred.
func (p *Painter) eraseRest(b *bytes.Buffer, x, y int, lastX, lastY *int) bool {
	if x >= p.cur.W {
		return false
	}
	want := p.cur.Cell(x, y)
	if want.R != ' ' || want.Comb != "" {
		return false
	}
	// EL paints with the current background, so it can only stand in for a run
	// of blanks that all share one style.
	for col := x; col < p.cur.W; col++ {
		c := p.cur.Cell(col, y)
		if c.R != ' ' || c.Comb != "" || !c.Style.Equal(want.Style) {
			return false
		}
	}
	// Only worth it if enough of the row actually needs repainting.
	const minRun = 8
	if p.cur.W-x < minRun {
		return false
	}
	// Erasing is pointless if the row is already blank there.
	if !p.full {
		identical := true
		for col := x; col < p.cur.W; col++ {
			if !p.same(col, y) {
				identical = false
				break
			}
		}
		if identical {
			return false
		}
	}

	if *lastY != y || *lastX != x {
		b.WriteString("\x1b[")
		b.WriteString(strconv.Itoa(y + 1))
		b.WriteByte(';')
		b.WriteString(strconv.Itoa(x + 1))
		b.WriteByte('H')
	}
	p.applyStyle(b, want.Style)
	b.WriteString("\x1b[K")
	for col := x; col < p.cur.W; col++ {
		p.prev.SetCell(col, y, want)
	}
	*lastX, *lastY = x, y
	return true
}

// applyStyle emits an SGR only when the terminal's state differs.
func (p *Painter) applyStyle(b *bytes.Buffer, s Style) {
	if p.styleSet && p.style.Equal(s) {
		return
	}
	b.WriteString(s.sequence(p.profile))
	p.style = s
	p.styleSet = true
}
