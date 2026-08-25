package render

import "unicode"

// Cell is one character position on the screen.
type Cell struct {
	// R is the base rune. A zero R marks the right-hand half of a double-width
	// character: the cell is occupied but nothing is drawn into it, and the
	// painter must not address it as a cursor destination.
	R rune
	// Comb holds combining marks that attach to R — the accent in a decomposed
	// "é", the variation selector and zero-width joiners inside an emoji
	// sequence. Kept as a string because it is empty for essentially every
	// cell, and an empty string costs no allocation, whereas a []rune field
	// would put a slice header in every cell of every frame.
	Comb  string
	Style Style
}

// blank is the cell an erased position holds.
func blank(s Style) Cell { return Cell{R: ' ', Style: s} }

// IsContinuation reports whether this cell is the second half of a wide rune.
func (c Cell) IsContinuation() bool { return c.R == 0 }

// Rect is a region of the screen, in cells.
type Rect struct {
	X, Y, W, H int
}

// Inset shrinks a rect by n on every side, never past empty.
func (r Rect) Inset(n int) Rect {
	out := Rect{X: r.X + n, Y: r.Y + n, W: r.W - 2*n, H: r.H - 2*n}
	if out.W < 0 {
		out.W = 0
	}
	if out.H < 0 {
		out.H = 0
	}
	return out
}

// Pad shrinks a rect by a different amount on each side.
func (r Rect) Pad(top, right, bottom, left int) Rect {
	out := Rect{X: r.X + left, Y: r.Y + top, W: r.W - left - right, H: r.H - top - bottom}
	if out.W < 0 {
		out.W = 0
	}
	if out.H < 0 {
		out.H = 0
	}
	return out
}

// Empty reports whether the rect has no area.
func (r Rect) Empty() bool { return r.W <= 0 || r.H <= 0 }

// Contains reports whether a point falls inside.
func (r Rect) Contains(x, y int) bool {
	return x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H
}

// Bottom is one past the last row.
func (r Rect) Bottom() int { return r.Y + r.H }

// Right is one past the last column.
func (r Rect) Right() int { return r.X + r.W }

// SplitTop cuts n rows off the top, returning them and the remainder.
func (r Rect) SplitTop(n int) (Rect, Rect) {
	if n > r.H {
		n = r.H
	}
	if n < 0 {
		n = 0
	}
	return Rect{r.X, r.Y, r.W, n}, Rect{r.X, r.Y + n, r.W, r.H - n}
}

// SplitBottom cuts n rows off the bottom, returning the remainder and them.
func (r Rect) SplitBottom(n int) (Rect, Rect) {
	if n > r.H {
		n = r.H
	}
	if n < 0 {
		n = 0
	}
	return Rect{r.X, r.Y, r.W, r.H - n}, Rect{r.X, r.Y + r.H - n, r.W, n}
}

// SplitLeft cuts n columns off the left, returning them and the remainder.
func (r Rect) SplitLeft(n int) (Rect, Rect) {
	if n > r.W {
		n = r.W
	}
	if n < 0 {
		n = 0
	}
	return Rect{r.X, r.Y, n, r.H}, Rect{r.X + n, r.Y, r.W - n, r.H}
}

// SplitRight cuts n columns off the right, returning the remainder and them.
func (r Rect) SplitRight(n int) (Rect, Rect) {
	if n > r.W {
		n = r.W
	}
	if n < 0 {
		n = 0
	}
	return Rect{r.X, r.Y, r.W - n, r.H}, Rect{r.X + r.W - n, r.Y, n, r.H}
}

// Buffer is a grid of cells: one frame's worth of screen.
//
// Every write is clipped to the buffer's bounds rather than panicking. A TUI
// computes coordinates from a terminal size that can change between the layout
// pass and the draw pass, and a renderer that panics on an off-by-one during a
// window resize is a renderer that takes the user's session with it.
type Buffer struct {
	W, H  int
	cells []Cell
}

// NewBuffer allocates a blank buffer.
func NewBuffer(w, h int) *Buffer {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	b := &Buffer{W: w, H: h, cells: make([]Cell, w*h)}
	b.Clear(Plain)
	return b
}

// Bounds is the whole buffer as a rect.
func (b *Buffer) Bounds() Rect { return Rect{0, 0, b.W, b.H} }

// Resize reshapes the buffer, discarding contents.
//
// Contents are discarded rather than reflowed because reflowing is a lie: the
// caller is about to redraw from its own model, and preserved cells that the
// new layout does not overwrite would be stale content presented as current.
func (b *Buffer) Resize(w, h int) {
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	if b.W == w && b.H == h {
		return
	}
	b.W, b.H = w, h
	if cap(b.cells) >= w*h {
		b.cells = b.cells[:w*h]
	} else {
		b.cells = make([]Cell, w*h)
	}
	b.Clear(Plain)
}

// Clear fills the whole buffer with blanks in the given style.
func (b *Buffer) Clear(s Style) {
	c := blank(s)
	for i := range b.cells {
		b.cells[i] = c
	}
}

// Cell returns the cell at x,y, or a blank if out of bounds.
func (b *Buffer) Cell(x, y int) Cell {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return blank(Plain)
	}
	return b.cells[y*b.W+x]
}

// SetCell writes one cell, clipped.
func (b *Buffer) SetCell(x, y int, c Cell) {
	if x < 0 || y < 0 || x >= b.W || y >= b.H {
		return
	}
	b.cells[y*b.W+x] = c
}

// SetRune writes a single rune and returns the columns it consumed.
//
// A double-width rune claims the cell to its right as a continuation. Writing
// one in the last column would leave half a glyph hanging off the edge, so it
// is replaced by a space: the terminal's own behaviour there is undefined and
// varies between wrapping, clipping, and corrupting the line.
func (b *Buffer) SetRune(x, y int, r rune, s Style) int {
	if y < 0 || y >= b.H || x >= b.W {
		return 0
	}
	w := RuneWidth(r)
	if w == 0 {
		// A control character is zero-width, but it is not a combining mark.
		// Appending it here was how an escape sequence reassembled itself on
		// the far side of the frame: RuneWidth answers 0 for ESC, the run below
		// hung it off the preceding cell's Comb, and the painter writes Comb to
		// the tty verbatim — so "a\x1b[31mX" arrived at the terminal as a
		// working SGR sequence with the buffer none the wiser. Untrusted text is
		// cleaned before it becomes an event, and this is the backstop for the
		// path that forgets: a control character can be dropped here without
		// losing anything, because there is no legitimate producer of one.
		if unicode.IsControl(r) || r < 0x20 {
			return 0
		}
		// A combining mark attaches to the cell on its left. With no cell to
		// its left there is nothing to attach to, and it is dropped rather
		// than promoted to a base character it is not.
		if x-1 >= 0 && x-1 < b.W {
			b.cells[y*b.W+x-1].Comb += string(r)
		}
		return 0
	}
	if x < 0 {
		return w
	}
	if w == 2 {
		if x+1 >= b.W {
			b.cells[y*b.W+x] = Cell{R: ' ', Style: s}
			return 1
		}
		b.cells[y*b.W+x] = Cell{R: r, Style: s}
		b.cells[y*b.W+x+1] = Cell{R: 0, Style: s}
		return 2
	}
	b.cells[y*b.W+x] = Cell{R: r, Style: s}
	return 1
}

// SetString writes a string left to right and returns the columns consumed.
func (b *Buffer) SetString(x, y int, text string, s Style) int {
	used := 0
	for _, r := range text {
		if x+used >= b.W {
			break
		}
		used += b.SetRune(x+used, y, r, s)
	}
	return used
}

// SetStringClipped writes a string bounded to max columns, adding an ellipsis
// when it does not fit.
func (b *Buffer) SetStringClipped(x, y int, text string, max int, s Style) int {
	return b.SetString(x, y, TruncateWidth(text, max, "…"), s)
}

// Fill paints a rect with one rune in one style.
func (b *Buffer) Fill(r Rect, ch rune, s Style) {
	for y := r.Y; y < r.Bottom(); y++ {
		for x := r.X; x < r.Right(); x++ {
			b.SetRune(x, y, ch, s)
		}
	}
}

// FillStyle repaints a rect's background without disturbing its runes.
//
// This is what makes a selected row, a focused pane, or a hovered item possible
// as a post-pass: the content is drawn once by whatever owns it, and the
// highlight is applied over the region afterwards.
func (b *Buffer) FillStyle(r Rect, s Style) {
	for y := r.Y; y < r.Bottom(); y++ {
		if y < 0 || y >= b.H {
			continue
		}
		for x := r.X; x < r.Right(); x++ {
			if x < 0 || x >= b.W {
				continue
			}
			b.cells[y*b.W+x].Style = s
		}
	}
}

// BlendBackground tints a rect's background toward c, leaving foregrounds and
// attributes alone. Used for selection bands that must not destroy the syntax
// or severity colouring underneath them.
func (b *Buffer) BlendBackground(r Rect, c Color, t float64) {
	for y := r.Y; y < r.Bottom(); y++ {
		if y < 0 || y >= b.H {
			continue
		}
		for x := r.X; x < r.Right(); x++ {
			if x < 0 || x >= b.W {
				continue
			}
			cell := &b.cells[y*b.W+x]
			cell.Style.Bg = cell.Style.Bg.Blend(c, t)
		}
	}
}
