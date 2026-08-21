package render

import (
	"io"
	"strings"
)

// Span is a run of text sharing one style.
type Span struct {
	Text  string
	Style Style
}

// Line is a styled line of text, built from spans.
//
// Scrollback content is authored as lines of spans rather than as pre-rendered
// strings with escapes embedded. The difference matters when the same content
// has to be wrapped to a new width, searched, copied to the clipboard, or
// re-styled as selected: escapes baked into a string have to be parsed back out
// to do any of that, and parsing escapes out of content is how a renderer
// starts executing the content it is displaying.
type Line []Span

// Text is the line's plain text.
func (l Line) Text() string {
	var b strings.Builder
	for _, s := range l {
		b.WriteString(s.Text)
	}
	return b.String()
}

// Width is the line's column count.
func (l Line) Width() int {
	w := 0
	for _, s := range l {
		w += StringWidth(s.Text)
	}
	return w
}

// Styled builds a single-span line.
func Styled(text string, s Style) Line { return Line{{Text: text, Style: s}} }

// Append adds a span, returning the extended line.
func (l Line) Append(text string, s Style) Line {
	if text == "" {
		return l
	}
	return append(l, Span{Text: text, Style: s})
}

// Concat joins two lines into one.
//
// A fresh slice rather than append-in-place: lines are cached and reused across
// frames, and appending into a shared backing array would let one frame's
// composition overwrite another's cached content.
func (l Line) Concat(other Line) Line {
	if len(other) == 0 {
		return l
	}
	out := make(Line, 0, len(l)+len(other))
	out = append(out, l...)
	out = append(out, other...)
	return out
}

// Pad extends the line to w columns with blanks in style s.
func (l Line) Pad(w int, s Style) Line {
	cur := l.Width()
	if cur >= w {
		return l
	}
	return l.Append(spaces(w-cur), s)
}

// Truncate cuts the line to w columns, marking the cut with an ellipsis in the
// style of the span it landed in.
func (l Line) Truncate(w int) Line {
	if w <= 0 {
		return nil
	}
	if l.Width() <= w {
		return l
	}
	budget := w - 1 // room for the ellipsis
	out := make(Line, 0, len(l))
	used := 0
	for _, span := range l {
		sw := StringWidth(span.Text)
		if used+sw <= budget {
			out = append(out, span)
			used += sw
			continue
		}
		cut := TruncateWidth(span.Text, budget-used, "")
		if cut != "" {
			out = append(out, Span{Text: cut, Style: span.Style})
		}
		out = append(out, Span{Text: "…", Style: span.Style})
		return out
	}
	return out
}

// Draw paints the line into a buffer at x,y and returns the columns consumed.
func (l Line) Draw(b *Buffer, x, y int) int {
	used := 0
	for _, span := range l {
		used += b.SetString(x+used, y, span.Text, span.Style)
	}
	return used
}

// DrawIn paints the line into the first row of a rect, clipped to its width.
func (l Line) DrawIn(b *Buffer, r Rect) {
	if r.Empty() {
		return
	}
	l.Truncate(r.W).Draw(b, r.X, r.Y)
}

// Wrap breaks a line to a width, preferring word boundaries.
//
// Wrapping happens on the styled representation so a span that straddles a
// break keeps its style on both sides. A wrapper that operated on plain text
// and re-applied styles afterwards would have to guess where the boundaries
// went, and gets it wrong on exactly the content that needs it: a tool result
// where the path is one colour and the message another.
//
// A word longer than the width — a URL, a base64 blob, a deep path — is broken
// mid-word rather than allowed to overflow. Overflow in a full-screen TUI does
// not just look wrong; it wraps in the terminal, which shifts every row below
// it and desynchronises the painter's model of the screen.
func (l Line) Wrap(width int) []Line {
	if width <= 0 {
		return nil
	}
	if l.Width() <= width {
		return []Line{l}
	}

	var out []Line
	var cur Line
	curWidth := 0
	// pending holds the word being accumulated, which may span several styles.
	var pending Line
	pendingWidth := 0

	flushWord := func() {
		cur = append(cur, pending...)
		curWidth += pendingWidth
		pending = nil
		pendingWidth = 0
	}
	breakLine := func() {
		out = append(out, cur)
		cur = nil
		curWidth = 0
	}

	for _, span := range l {
		for _, word := range splitKeepSpace(span.Text) {
			ww := StringWidth(word)
			isSpace := strings.TrimSpace(word) == "" && word != ""

			if isSpace {
				// Whitespace is a break opportunity: commit the pending word,
				// then keep the space only if the line is not already full.
				flushWord()
				if curWidth+ww <= width {
					cur = cur.Append(word, span.Style)
					curWidth += ww
				} else if curWidth > 0 {
					breakLine()
				}
				continue
			}

			// A word too long for any line is hard-broken.
			if pendingWidth+ww > width {
				flushWord()
				if curWidth > 0 {
					breakLine()
				}
				for StringWidth(word) > width {
					head := TruncateWidth(word, width, "")
					cur = cur.Append(head, span.Style)
					breakLine()
					word = word[len(head):]
				}
				ww = StringWidth(word)
			}

			if curWidth+pendingWidth+ww > width {
				if curWidth > 0 {
					// The pending word moves down whole rather than being split
					// at the margin.
					held := pending
					heldWidth := pendingWidth
					pending, pendingWidth = nil, 0
					breakLine()
					pending, pendingWidth = held, heldWidth
				}
			}
			if word != "" {
				pending = pending.Append(word, span.Style)
				pendingWidth += ww
			}
		}
	}
	flushWord()
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	// Trailing whitespace on a wrapped row is invisible but occupies columns,
	// which matters when the row is later padded or highlighted.
	for i := range out {
		out[i] = trimTrailingSpace(out[i])
	}
	return out
}

// splitKeepSpace splits text into words and the whitespace runs between them,
// keeping both, so wrapping can preserve intentional spacing in tables and
// indented output.
func splitKeepSpace(text string) []string {
	if text == "" {
		return nil
	}
	var out []string
	start := 0
	inSpace := text[0] == ' ' || text[0] == '\t'
	for i := 0; i < len(text); i++ {
		s := text[i] == ' ' || text[i] == '\t'
		if s != inSpace {
			out = append(out, text[start:i])
			start = i
			inSpace = s
		}
	}
	return append(out, text[start:])
}

func trimTrailingSpace(l Line) Line {
	for len(l) > 0 {
		last := l[len(l)-1]
		trimmed := strings.TrimRight(last.Text, " \t")
		if trimmed == last.Text {
			return l
		}
		if trimmed == "" {
			l = l[:len(l)-1]
			continue
		}
		l[len(l)-1].Text = trimmed
		return l
	}
	return l
}

// WrapText wraps plain text in one style, splitting on newlines first.
func WrapText(text string, width int, s Style) []Line {
	var out []Line
	for _, raw := range strings.Split(text, "\n") {
		out = append(out, Styled(raw, s).Wrap(width)...)
	}
	return out
}

// WriteLines writes styled lines to an ordinary stream, one per line.
//
// It is the line-oriented counterpart to the painter, for the places that print
// a styled fragment into a stream they do not own — a mark on stdout, a fixture
// in a test — rather than composing a frame. No cursor movement, no diffing,
// and a reset at the end of every line so a caller cannot leave the stream
// styled.
//
// Styling follows the painter's rule, which means a NoColor profile still
// carries attributes. A caller writing to a pipe that should be entirely plain
// writes Line.Text instead; that decision belongs to the caller, because only
// it knows what its destination is.
func WriteLines(w io.Writer, p Profile, lines []Line) error {
	var b strings.Builder
	for _, l := range lines {
		for _, span := range l {
			b.WriteString(span.Style.sequence(p))
			b.WriteString(span.Text)
		}
		b.WriteString("\x1b[0m\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}
