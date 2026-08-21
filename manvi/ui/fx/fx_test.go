package fx

import (
	"strings"
	"testing"

	"manvi/ui/render"
)

func TestPulseIsBoundedAndPeriodic(t *testing.T) {
	const period = 20
	for tick := -3 * period; tick < 3*period; tick++ {
		v := Pulse(tick, period)
		if v < 0 || v > 1 {
			t.Fatalf("Pulse(%d) = %v, outside [0,1]", tick, v)
		}
		if v != Pulse(tick+period, period) {
			t.Fatalf("Pulse(%d) != Pulse(%d): not periodic", tick, tick+period)
		}
	}
	if got := Pulse(period/2, period); got != 1 {
		t.Fatalf("the crest of the wave is %v, want 1", got)
	}
	if got := Pulse(0, period); got != 0 {
		t.Fatalf("the wave starts at %v, want 0 — a pulse that begins lit never rests", got)
	}
	if got := Pulse(3, 1); got != 0 {
		t.Fatalf("a degenerate period returned %v", got)
	}
}

func TestRevealIsMonotonicAndComplete(t *testing.T) {
	const n = 40
	prev := 0
	for tick := 0; tick < 100; tick++ {
		got := Reveal(n, tick, 0, 2)
		if got < prev {
			t.Fatalf("Reveal went backwards at tick %d: %d < %d", tick, got, prev)
		}
		prev = got
	}
	if prev != n {
		t.Fatalf("after 100 ticks only %d of %d runes were revealed", prev, n)
	}
	if got := Reveal(n, 3, 10, 2); got != 0 {
		t.Fatalf("a reveal that has not started shows %d runes", got)
	}
}

func TestRevealBoundsLongTextsRatherThanDelayingThem(t *testing.T) {
	// A 10k-rune paste at the base rate would take minutes. The reveal is a
	// courtesy, so the rate adapts and the bound holds.
	n := 10_000
	if got := Reveal(n, maxRevealTicks, 0, 2); got != n {
		t.Fatalf("a long text took past the bound: %d of %d at the deadline", got, n)
	}
}

func TestRevealTextNeverSplitsAMultiByteRune(t *testing.T) {
	s := "日本語のテキストです"
	for tick := 0; tick < 30; tick++ {
		out := RevealText(s, tick, 0, 1)
		if strings.ContainsRune(out, 0xFFFD) {
			t.Fatalf("tick %d produced a broken rune: %q", tick, out)
		}
	}
	if got := RevealText(s, 500, 0, 1); got != s {
		t.Fatalf("the completed reveal is %q", got)
	}
}

func TestMarqueeScrollsAndDwells(t *testing.T) {
	text := "a label far too long for its strip"
	const width = 10
	if got := Marquee(text, width, 0); got != text[:width] {
		t.Fatalf("the marquee dwells at its start; got %q", got)
	}
	seen := map[string]bool{}
	for tick := 0; tick < 200; tick++ {
		got := Marquee(text, width, tick)
		if len([]rune(got)) != width {
			t.Fatalf("tick %d: window is %d runes, want %d", tick, len([]rune(got)), width)
		}
		if !strings.Contains(text, got) {
			t.Fatalf("tick %d: %q is not a window of the label", tick, got)
		}
		seen[got] = true
	}
	if len(seen) < 3 {
		t.Fatal("the marquee never scrolled")
	}
	if got := Marquee("fits", 10, 7); got != "fits" {
		t.Fatalf("a short label scrolled to %q", got)
	}
}

func TestSeriesIsARing(t *testing.T) {
	s := NewSeries(4)
	if s.Len() != 0 {
		t.Fatal("a new ring reports samples")
	}
	for i := 1; i <= 10; i++ {
		s.Push(float64(i))
	}
	got := s.Values()
	want := []float64{7, 8, 9, 10}
	if len(got) != len(want) {
		t.Fatalf("Values() = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Values() = %v, want %v", got, want)
		}
	}
	// Mutating the copy must not disturb the ring.
	got[0] = -1
	if s.Values()[0] != 7 {
		t.Fatal("Values() shares memory with the ring")
	}
	if NewSeries(0).Len() != 0 {
		t.Fatal("a zero-capacity ring misbehaved")
	}
}

func TestRainIsDeterministicAndStaysInsideItsRect(t *testing.T) {
	rain := Rain{Gap: 3, Tail: 8}
	r := render.Rect{X: 2, Y: 1, W: 30, H: 12}
	draw := func(tick int) *render.Buffer {
		b := render.NewBuffer(40, 16)
		b.Fill(render.Rect{X: 0, Y: 0, W: 40, H: 16}, ' ', render.Plain)
		rain.Draw(b, r, tick, render.RGB(0, 255, 0), render.RGB(0, 0, 0), true)
		return b
	}
	a, b2 := draw(7), draw(7)
	later := draw(8)
	moved := false
	for y := 0; y < 16; y++ {
		for x := 0; x < 40; x++ {
			in := r.Contains(x, y)
			if !in && a.Cell(x, y).R != ' ' {
				t.Fatalf("rain painted outside its rect at %d,%d", x, y)
			}
			if a.Cell(x, y) != b2.Cell(x, y) {
				t.Fatalf("tick 7 drawn twice differs at %d,%d", x, y)
			}
			if in && a.Cell(x, y) != later.Cell(x, y) {
				moved = true
			}
		}
	}
	if !moved {
		t.Fatal("the field did not move between ticks")
	}
}

func TestRainFallsBackToASCIIWithoutUnicode(t *testing.T) {
	b := render.NewBuffer(20, 10)
	Rain{Gap: 2, Tail: 6}.Draw(b, render.Rect{X: 0, Y: 0, W: 20, H: 10}, 30,
		render.RGB(0, 255, 0), render.RGB(0, 0, 0), false)
	for y := 0; y < 10; y++ {
		for x := 0; x < 20; x++ {
			if r := b.Cell(x, y).R; r > 127 {
				t.Fatalf("non-ASCII glyph %q in the fallback set at %d,%d", r, x, y)
			}
		}
	}
}

func TestSweepStyledKeepsColdCellsInTheColdStyle(t *testing.T) {
	b := render.NewBuffer(20, 3)
	cold := render.Style{Fg: render.RGB(40, 40, 40), Bg: render.RGB(10, 10, 10)}
	hot := render.RGB(255, 200, 0)
	r := render.Rect{X: 0, Y: 1, W: 20, H: 1}
	SweepStyled(b, r, 5, "─", hot, cold)
	for x := 0; x < 20; x++ {
		c := b.Cell(x, 1)
		if c.R != '─' {
			t.Fatalf("cell %d is %q, want the border glyph", x, c.R)
		}
		if c.Style.Bg != cold.Bg {
			t.Fatalf("cell %d changed background: the beam left a trail", x)
		}
	}
	if b.Cell(5, 1).Style.Fg != hot {
		t.Fatal("the head of the beam is not the hot colour")
	}
	// A cell far ahead of the head is the cold style exactly.
	if got := b.Cell(15, 1).Style; !got.Equal(cold) {
		t.Fatalf("a cell the wave has not reached changed: %+v", got)
	}
}

func TestGradientSweepKeepsTheText(t *testing.T) {
	got := GradientSweep("manvi", render.Hex("#ff0000"), render.Hex("#0000ff"), 9, render.Bold)
	if got.Text() != "manvi" {
		t.Fatalf("the sweep rewrote its text: %q", got.Text())
	}
	if GradientSweep("", render.Hex("#fff"), render.Hex("#000"), 0, 0) != nil {
		t.Fatal("an empty string produced spans")
	}
}
