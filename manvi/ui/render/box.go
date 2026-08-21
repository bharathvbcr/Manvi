package render

import "strings"

// Border is the set of glyphs a box is drawn from.
type Border struct {
	TopLeft, Top, TopRight          string
	Left, Right                     string
	BottomLeft, Bottom, BottomRight string
	// TeeLeft and TeeRight join an interior horizontal rule to the frame.
	TeeLeft, TeeRight string
}

// The border sets. Rounded is the default because square corners at this weight
// read as heavier than the content they contain.
var (
	Rounded = Border{"╭", "─", "╮", "│", "│", "╰", "─", "╯", "├", "┤"}
	Square  = Border{"┌", "─", "┐", "│", "│", "└", "─", "┘", "├", "┤"}
	Thick   = Border{"┏", "━", "┓", "┃", "┃", "┗", "━", "┛", "┣", "┫"}
	Double  = Border{"╔", "═", "╗", "║", "║", "╚", "═", "╝", "╠", "╣"}
	// ASCII is the fallback for a terminal that cannot be trusted with box
	// drawing — a raw Windows console, a serial line, LANG=C. A box made of
	// question marks is worse than a box made of pipes.
	ASCII = Border{"+", "-", "+", "|", "|", "+", "-", "+", "+", "+"}
	// HalfBlock draws with block elements, for panels that want weight on one
	// side only.
	HalfBlock = Border{"▛", "▀", "▜", "▌", "▐", "▙", "▄", "▟", "▌", "▐"}
)

// Box describes a framed region.
type Box struct {
	Border Border
	Style  Style
	// Title is drawn into the top edge, left-aligned after one glyph of inset.
	Title Line
	// Subtitle is drawn into the bottom edge, right-aligned.
	Subtitle Line
	// Fill, when set, paints the interior background before content is drawn.
	Fill *Style
}

// Draw paints the frame and returns the interior rect.
//
// A rect too small for a frame gets no frame and the whole rect as interior:
// during a resize the layout can briefly ask for a two-column box, and drawing
// two border glyphs with nothing between them turns a transient narrow window
// into a permanently unreadable one.
func (b Box) Draw(buf *Buffer, r Rect) Rect {
	if r.W < 2 || r.H < 2 {
		return r
	}
	bd := b.Border
	inner := r.Inset(1)

	if b.Fill != nil {
		buf.Fill(inner, ' ', *b.Fill)
	}

	buf.SetString(r.X, r.Y, bd.TopLeft, b.Style)
	buf.SetString(r.Right()-1, r.Y, bd.TopRight, b.Style)
	buf.SetString(r.X, r.Bottom()-1, bd.BottomLeft, b.Style)
	buf.SetString(r.Right()-1, r.Bottom()-1, bd.BottomRight, b.Style)

	for x := r.X + 1; x < r.Right()-1; x++ {
		buf.SetString(x, r.Y, bd.Top, b.Style)
		buf.SetString(x, r.Bottom()-1, bd.Bottom, b.Style)
	}
	for y := r.Y + 1; y < r.Bottom()-1; y++ {
		buf.SetString(r.X, y, bd.Left, b.Style)
		buf.SetString(r.Right()-1, y, bd.Right, b.Style)
	}

	if len(b.Title) > 0 && r.W > 6 {
		title := b.Title.Truncate(r.W - 6)
		buf.SetString(r.X+2, r.Y, " ", b.Style)
		used := title.Draw(buf, r.X+3, r.Y)
		buf.SetString(r.X+3+used, r.Y, " ", b.Style)
	}
	if len(b.Subtitle) > 0 && r.W > 6 {
		sub := b.Subtitle.Truncate(r.W - 6)
		w := sub.Width()
		x := r.Right() - 3 - w
		buf.SetString(x-1, r.Bottom()-1, " ", b.Style)
		sub.Draw(buf, x, r.Bottom()-1)
		buf.SetString(x+w, r.Bottom()-1, " ", b.Style)
	}
	return inner
}

// Spinner frames, in ascending visual weight. Braille reads as motion at any
// speed; the ASCII fallback is for terminals without the glyphs.
var (
	SpinnerDots  = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	SpinnerASCII = []string{"|", "/", "-", "\\"}
)

// Spinner picks a frame for a tick count.
func Spinner(frames []string, tick int) string {
	if len(frames) == 0 {
		return ""
	}
	i := tick % len(frames)
	if i < 0 {
		i += len(frames)
	}
	return frames[i]
}

// barBlocks are the eighth-width block elements, which give a progress bar
// eight times the resolution of its column count.
var barBlocks = []string{"", "▏", "▎", "▍", "▌", "▋", "▊", "▉"}

// ProgressBar renders a bar of w columns at fraction f, sub-column accurate.
func ProgressBar(w int, f float64, filled, empty Style) Line {
	if w <= 0 {
		return nil
	}
	if f < 0 {
		f = 0
	}
	if f > 1 {
		f = 1
	}
	eighths := int(f*float64(w)*8 + 0.5)
	full := eighths / 8
	part := eighths % 8
	if full > w {
		full, part = w, 0
	}
	line := Line{}
	if full > 0 {
		line = line.Append(strings.Repeat("█", full), filled)
	}
	rest := w - full
	if part > 0 && rest > 0 {
		// The partial block is drawn in the filled colour on the empty
		// background, so the bar reads as one shape rather than two.
		line = line.Append(barBlocks[part], Style{Fg: filled.Fg, Bg: empty.Bg, Attrs: filled.Attrs})
		rest--
	}
	if rest > 0 {
		line = line.Append(strings.Repeat("░", rest), empty)
	}
	return line
}

// GradientLine renders text with its foreground swept from one colour to
// another across the string.
//
// Each rune becomes its own span, which is why this is reserved for short
// strings — a banner, a heading. On a truecolor terminal it is a smooth ramp;
// on a 16-colour one the reduction collapses it to a few steps, which still
// reads as intentional rather than broken.
func GradientLine(text string, from, to Color, attrs Attr) Line {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	out := make(Line, 0, len(runes))
	denom := float64(len(runes) - 1)
	for i, r := range runes {
		t := 0.0
		if denom > 0 {
			t = float64(i) / denom
		}
		out = append(out, Span{Text: string(r), Style: Style{Fg: from.Blend(to, t), Bg: Default, Attrs: attrs}})
	}
	return out
}

// sparkBlocks are the eighth-height block elements.
var sparkBlocks = []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}

// Sparkline renders a series as one row of block elements, scaled to its own
// maximum. Used for token-rate and step-duration strips in the status bar.
func Sparkline(values []float64, s Style) Line {
	if len(values) == 0 {
		return nil
	}
	max := values[0]
	for _, v := range values {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		max = 1
	}
	var b strings.Builder
	for _, v := range values {
		i := int(v / max * float64(len(sparkBlocks)-1))
		if i < 0 {
			i = 0
		}
		if i >= len(sparkBlocks) {
			i = len(sparkBlocks) - 1
		}
		b.WriteRune(sparkBlocks[i])
	}
	return Styled(b.String(), s)
}
