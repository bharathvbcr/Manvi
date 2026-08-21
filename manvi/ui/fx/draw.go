package fx

import (
	"manvi/ui/render"
)

// SweepStyled paints a travelling highlight across a single row — the top
// edge of a bordered box, in practice — so a frame whose contents are
// momentarily idle still says that work is running underneath it.
//
// The head is the hot colour; behind it a short tail fades back to the cold
// style, and every other cell is the cold style unchanged, so the sweep can
// run over a border without leaving a trail of mismatched backgrounds behind
// it. The painter's diff for one tick of this is the tail plus the cell the
// head vacated: a beam is the cheapest motion there is, which is why it is
// the right ambient cue.
func SweepStyled(b *render.Buffer, r render.Rect, tick int, glyph string, hot render.Color, cold render.Style) {
	if r.Empty() || glyph == "" {
		return
	}
	w := r.W
	head := tick % w
	if head < 0 {
		head += w
	}
	const tail = 8
	for x := 0; x < w; x++ {
		d := (head - x + w) % w
		c := cold
		if d < tail {
			c.Fg = hot.Blend(cold.Fg, float64(d)/float64(tail))
		}
		b.SetString(r.X+x, r.Y, glyph, c)
	}
}

// Rain is the digital-rain backdrop for a screen with nothing on it yet.
//
// One stream per Gap columns; a column's speed and phase are hashed from its
// index, so the whole field is a pure function of the tick — no particles,
// no slices, no state to reset on a resize. A cell's glyph reshuffles only
// while the wave is passing it, so the per-frame diff is the moving heads and
// their fading tails rather than the field.
type Rain struct {
	// Gap is the column stride between streams. 3 reads as rain; 1 is noise.
	Gap int
	// Tail is the fade length in rows behind each head.
	Tail int
}

// katakana are the halfwidth forms, which the width tables class as narrow.
// The fullwidth forms are two cells each and would tear the field's lattice.
var katakana = []string{
	"ｱ", "ｲ", "ｳ", "ｴ", "ｵ", "ｶ", "ｷ", "ｸ", "ｹ", "ｺ",
	"ｻ", "ｼ", "ｽ", "ｾ", "ｿ", "ﾀ", "ﾁ", "ﾂ", "ﾃ", "ﾄ",
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
}

// asciiRain is the fallback for a terminal that cannot be trusted with the
// katakana set.
var asciiRain = []string{
	"0", "1", "2", "3", "4", "5", "6", "7", "8", "9",
	"a", "b", "c", "d", "e", "f", "z", "x", "=", "+",
}

// Draw paints the field into area. hot is the head colour; cold the colour a
// faded cell reaches, normally the pane background.
func (r Rain) Draw(b *render.Buffer, area render.Rect, tick int, hot, cold render.Color, unicode bool) {
	if area.Empty() {
		return
	}
	gap := r.Gap
	if gap < 1 {
		gap = 3
	}
	tail := r.Tail
	if tail < 2 {
		tail = 8
	}
	glyphs := asciiRain
	if unicode {
		glyphs = katakana
	}
	// A head travels the field's height plus twice its tail, so a stream
	// fully leaves before it re-enters: the gap between passes is what keeps
	// the field sparse.
	span := area.H + tail*2
	for x := area.X; x < area.Right(); x++ {
		col := x - area.X
		if col%gap != 0 {
			continue
		}
		h := hash(col)
		speed := 1 + int(h&1)
		phase := int(h >> 8)
		headRow := (phase + tick*speed) % span
		for y := area.Y; y < area.Bottom(); y++ {
			d := headRow - (y - area.Y)
			if d < 0 || d >= tail {
				continue
			}
			style := render.Style{Fg: hot.Blend(cold, float64(d)/float64(tail)), Bg: cold}
			if d == 0 {
				// The head is the one lit cell of a stream.
				style = render.Style{Fg: hot, Bg: cold, Attrs: render.Bold}
			}
			b.SetString(x, y, glyphs[glyphIndex(col, y-area.Y, tick, len(glyphs))], style)
		}
	}
}

// hash decorrelates neighbouring columns: a multiplicative mix so that the
// streams do not fall in lockstep.
func hash(n int) uint32 {
	x := uint32(n)*2654435761 + 0x9e3779b9
	x ^= x >> 13
	x *= 0x85ebca6b
	x ^= x >> 16
	return x
}

// glyphIndex picks a cell's glyph. The tick term advances only every fourth
// tick, so a cell shimmers while the wave passes through it rather than
// strobing every frame.
func glyphIndex(col, row, tick, n int) int {
	return int(hash(col*7919+row*104729+tick/4) % uint32(n))
}

// GradientSweep renders text as a colour ramp with a wave of the destination
// colour travelling through it — a static gradient that breathes. The text
// itself never moves, so the only diff one tick produces is a handful of
// recoloured cells.
func GradientSweep(text string, from, to render.Color, tick int, attrs render.Attr) render.Line {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	out := make(render.Line, 0, len(runes))
	n := float64(len(runes))
	// The wave crosses the text over waveTicks ticks and wraps; the +3 lets
	// the crest exit fully rather than popping off the last glyph.
	const waveTicks = 48
	head := float64(tick%waveTicks) / waveTicks * (n + 3)
	for i, r := range runes {
		t := 0.0
		if n > 1 {
			t = float64(i) / (n - 1)
		}
		c := from.Blend(to, t)
		d := float64(i) - head
		if d < 0 {
			d = -d
		}
		if d < 3 {
			c = c.Blend(to, (1-d/3)*0.7)
		}
		out = append(out, render.Span{Text: string(r), Style: render.Style{Fg: c, Bg: render.Default, Attrs: attrs}})
	}
	return out
}
