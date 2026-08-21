package render

import "strings"

// Attr is a bitmask of text attributes.
type Attr uint16

// The attributes the painter can emit. Blink is deliberately absent: it is
// disruptive, widely disabled, and there is no state in a harness worth
// signalling with it.
const (
	Bold Attr = 1 << iota
	Dim
	Italic
	Underline
	Reverse
	Strike
)

// Style is a cell's full presentation.
//
// It is a value type with no pointers, which matters more than it looks: a
// screen buffer is a flat slice of cells and each cell carries its style, so a
// frame is one allocation rather than one per styled run.
type Style struct {
	Fg    Color
	Bg    Color
	Attrs Attr
}

// Plain is the terminal's own colours with no attributes.
var Plain = Style{Fg: Default, Bg: Default}

// With returns a copy carrying the given attributes.
func (s Style) With(a Attr) Style { s.Attrs |= a; return s }

// Without returns a copy with the given attributes cleared.
func (s Style) Without(a Attr) Style { s.Attrs &^= a; return s }

// Foreground returns a copy with the foreground replaced.
func (s Style) Foreground(c Color) Style { s.Fg = c; return s }

// Background returns a copy with the background replaced.
func (s Style) Background(c Color) Style { s.Bg = c; return s }

// Has reports whether every attribute in a is set.
func (s Style) Has(a Attr) bool { return s.Attrs&a == a }

// Equal reports whether two styles would paint identically.
func (s Style) Equal(o Style) bool {
	return s.Fg == o.Fg && s.Bg == o.Bg && s.Attrs == o.Attrs
}

// sequence renders the full SGR for this style, resetting first.
//
// Every transition resets rather than computing the minimal delta from the
// previous style. The delta is tempting and it is where terminal renderers
// acquire their worst bugs: attributes have no reliable individual "off" codes
// across emulators — 21 is double-underline on some and bold-off on others —
// so a cleared attribute can persist and bleed styling across the rest of the
// frame. Resetting costs four bytes and cannot bleed.
func (s Style) sequence(p Profile) string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	if s.Attrs&Bold != 0 {
		b.WriteString(";1")
	}
	if s.Attrs&Dim != 0 {
		b.WriteString(";2")
	}
	if s.Attrs&Italic != 0 {
		b.WriteString(";3")
	}
	if s.Attrs&Underline != 0 {
		b.WriteString(";4")
	}
	if s.Attrs&Reverse != 0 {
		b.WriteString(";7")
	}
	if s.Attrs&Strike != 0 {
		b.WriteString(";9")
	}
	s.Fg.sgr(&b, p, true)
	s.Bg.sgr(&b, p, false)
	b.WriteString("m")
	return b.String()
}
