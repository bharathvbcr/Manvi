package render

import (
	"bytes"
	"strings"
	"testing"
)

func TestRuneWidth(t *testing.T) {
	cases := []struct {
		r    rune
		want int
		why  string
	}{
		{'a', 1, "ascii"},
		{'é', 1, "precomposed latin"},
		{0x0301, 0, "combining acute attaches to the rune before it"},
		{'漢', 2, "CJK ideograph"},
		{'Ａ', 2, "fullwidth latin, U+FF21, in the FF01-FF60 block"},
		{'ｱ', 1, "halfwidth katakana, U+FF71, is narrow despite living near the fullwidth block"},
		{'ア', 2, "ordinary katakana"},
		{0x1F600, 2, "emoji"},
		{0x200D, 0, "zero-width joiner"},
		{0xFE0F, 0, "variation selector"},
		{'\x1b', 0, "escape has no width; it should never reach a buffer"},
		{'→', 1, "arrow is narrow"},
	}
	for _, c := range cases {
		if got := RuneWidth(c.r); got != c.want {
			t.Errorf("RuneWidth(%#U) = %d, want %d (%s)", c.r, got, c.want, c.why)
		}
	}
}

func TestStringWidthCountsColumnsNotRunes(t *testing.T) {
	// The bug this guards: a status bar sized by rune count overflows and wraps,
	// which shifts every row of a full-screen frame.
	s := "漢字 mixed"
	if got, want := StringWidth(s), 4+1+5; got != want {
		t.Fatalf("StringWidth(%q) = %d, want %d", s, got, want)
	}
	if got := len([]rune(s)); got == StringWidth(s) {
		t.Fatal("rune count and column count agree; the case is not exercising wide runes")
	}
}

func TestTruncateWidthCutsByColumn(t *testing.T) {
	if got, want := TruncateWidth("漢字漢字", 5, "…"), "漢字…"; got != want {
		t.Fatalf("TruncateWidth = %q, want %q", got, want)
	}
	if got := StringWidth(TruncateWidth("漢字漢字", 5, "…")); got > 5 {
		t.Fatalf("truncation produced %d columns, over the 5 asked for", got)
	}
	if got, want := TruncateWidth("short", 20, "…"), "short"; got != want {
		t.Fatalf("TruncateWidth on a fitting string = %q, want %q", got, want)
	}
}

func TestHexParsing(t *testing.T) {
	r, g, b, ok := Hex("#1f6feb").RGBA()
	if !ok || r != 0x1f || g != 0x6f || b != 0xeb {
		t.Fatalf("Hex(#1f6feb) = %d,%d,%d ok=%v", r, g, b, ok)
	}
	r, g, b, _ = Hex("#abc").RGBA()
	if r != 0xaa || g != 0xbb || b != 0xcc {
		t.Fatalf("Hex(#abc) = %d,%d,%d, want aa bb cc", r, g, b)
	}
	if !Hex("not a colour").IsDefault() {
		t.Fatal("an unparseable colour must fall back to Default, not fail the UI")
	}
}

func TestColorReductionStaysInProfile(t *testing.T) {
	// A truecolor sequence sent to a 16-colour terminal is printed as text on
	// several emulators, which corrupts the frame rather than degrading it.
	var b strings.Builder
	RGB(0x1f, 0x6f, 0xeb).sgr(&b, ANSI16, true)
	if strings.Contains(b.String(), ";2;") {
		t.Fatalf("ANSI16 profile emitted a truecolor sequence: %q", b.String())
	}

	b.Reset()
	RGB(0x1f, 0x6f, 0xeb).sgr(&b, ANSI256, true)
	if !strings.Contains(b.String(), ";38;5;") {
		t.Fatalf("ANSI256 profile did not emit an indexed sequence: %q", b.String())
	}

	b.Reset()
	RGB(0x1f, 0x6f, 0xeb).sgr(&b, TrueColor, true)
	if !strings.Contains(b.String(), ";38;2;31;111;235") {
		t.Fatalf("TrueColor profile lost the exact colour: %q", b.String())
	}

	b.Reset()
	RGB(0x1f, 0x6f, 0xeb).sgr(&b, NoColor, true)
	if b.String() != "" {
		t.Fatalf("NoColor profile emitted colour: %q", b.String())
	}
}

// A colour with a hue must arrive at a 16-colour terminal with a hue.
//
// It used to arrive as grey. Two faults compounded: an RGB colour was rounded
// into the 256-colour cube first, which lifts a saturated colour's lesser
// channels toward its greatest and drains the chroma the second rounding needs,
// and the second rounding then measured every one of the sixteen the same way —
// and grey sits in the middle of the cube, so it wins for anything that is not
// nearly full-strength. The palette's danger red, success green and amber grant
// all reduced to bright black.
//
// This asserts the whole path, sgr at ANSI16, rather than nearest16 alone: the
// cube stop-over was in sgr, and a nearest16 test would have passed throughout.
func TestANSI16KeepsAHueAHue(t *testing.T) {
	for _, c := range []struct {
		name    string
		col     Color
		want    string
		wantErr string
	}{
		{"the danger red", RGB(0xf8, 0x51, 0x49), ";91", "bright red"},
		{"the success green", RGB(0x3f, 0xb9, 0x50), ";32", "green"},
		{"the amber grant", RGB(0xdb, 0x9d, 0x47), ";33", "yellow"},
		{"the dark olive warning", RGB(0x9a, 0x67, 0x00), ";33", "yellow"},
		{"a muted teal", RGB(0x3f, 0x8f, 0x8f), ";36", "cyan"},
		{"a deep crimson, too dark for a hue", RGB(0x5c, 0x14, 0x21), ";30", "black"},
	} {
		var b strings.Builder
		c.col.sgr(&b, ANSI16, true)
		if got := b.String(); got != c.want {
			t.Errorf("%s reduced to %q, want %q (%s)", c.name, got, c.want, c.wantErr)
		}
	}
}

// The other half of the same boundary: a grey must not acquire a hue it never
// had. Splitting the sixteen into hues and greys is only correct if colours
// cross the line in one direction as rarely as the other.
func TestANSI16KeepsAGreyGrey(t *testing.T) {
	for _, c := range []struct {
		name string
		col  Color
		want string
	}{
		{"a near-black surface", RGB(0x0d, 0x11, 0x17), ";30"},
		{"a blue-tinted border", RGB(0x30, 0x36, 0x3d), ";30"},
		{"subtle text", RGB(0x6e, 0x76, 0x81), ";90"},
		{"a neutral mid grey", RGB(0x80, 0x80, 0x80), ";90"},
		{"near-white body text", RGB(0xe6, 0xed, 0xf3), ";37"},
	} {
		var b strings.Builder
		c.col.sgr(&b, ANSI16, true)
		if got := b.String(); got != c.want {
			t.Errorf("%s reduced to %q, want the grey slot %q", c.name, got, c.want)
		}
	}
}

func TestGreyReducesToTheGreyRampNotTheCube(t *testing.T) {
	// The cube has no neutral entry near 0x80, so a cube-only search tints
	// mid-greys visibly. The grey ramp must win here.
	idx := nearest256(0x80, 0x80, 0x80)
	if idx < 232 {
		r, g, b := paletteRGB(idx)
		t.Fatalf("mid grey reduced to cube index %d (%d,%d,%d); the grey ramp is closer", idx, r, g, b)
	}
}

func TestBlendWithDefaultReturnsTheKnownColour(t *testing.T) {
	// Default has no components, so there is nothing to interpolate toward.
	c := RGB(10, 20, 30).Blend(Default, 0.5)
	r, g, b, ok := c.RGBA()
	if !ok || r != 10 || g != 20 || b != 30 {
		t.Fatalf("blend toward Default = %d,%d,%d ok=%v, want the original colour", r, g, b, ok)
	}
	if !Default.Blend(Default, 0.5).IsDefault() {
		t.Fatal("blending two Defaults must stay Default")
	}
}

func TestBufferClipsRatherThanPanics(t *testing.T) {
	// Coordinates are computed from a terminal size that can change between
	// layout and draw. A panic there takes the user's session with it.
	b := NewBuffer(10, 3)
	b.SetString(-5, 1, "left overflow", Plain)
	b.SetString(8, 1, "right overflow", Plain)
	b.SetRune(0, 99, 'x', Plain)
	b.SetCell(99, 99, Cell{R: 'x'})
	b.Fill(Rect{-3, -3, 40, 40}, '#', Plain)
	if b.Cell(9, 2).R != '#' {
		t.Fatal("a clipped fill did not reach the last in-bounds cell")
	}
}

func TestWideRuneClaimsTwoCellsAndNeverStraddlesTheEdge(t *testing.T) {
	b := NewBuffer(4, 1)
	if got := b.SetRune(0, 0, '漢', Plain); got != 2 {
		t.Fatalf("wide rune consumed %d columns, want 2", got)
	}
	if !b.Cell(1, 0).IsContinuation() {
		t.Fatal("the cell right of a wide rune must be a continuation")
	}
	// Last column: half a glyph cannot be drawn, so a space stands in.
	if got := b.SetRune(3, 0, '漢', Plain); got != 1 {
		t.Fatalf("wide rune in the last column consumed %d columns, want 1", got)
	}
	if b.Cell(3, 0).R != ' ' {
		t.Fatalf("last column holds %q, want a space rather than half a glyph", b.Cell(3, 0).R)
	}
}

func TestCombiningMarkAttachesToThePreviousCell(t *testing.T) {
	b := NewBuffer(4, 1)
	b.SetString(0, 0, "éx", Plain)
	if got := b.Cell(0, 0); got.R != 'e' || got.Comb != "́" {
		t.Fatalf("cell 0 = %q+%q, want 'e' with the combining acute attached", got.R, got.Comb)
	}
	if b.Cell(1, 0).R != 'x' {
		t.Fatal("the combining mark consumed a column it should not have")
	}
}

func TestWrapKeepsStylesAcrossTheBreak(t *testing.T) {
	red := Plain.Foreground(ANSI(1))
	line := Styled("hello ", Plain).Append("worldly words here", red)
	got := line.Wrap(12)
	if len(got) < 2 {
		t.Fatalf("expected a wrap, got %d line(s)", len(got))
	}
	for i, l := range got {
		if l.Width() > 12 {
			t.Fatalf("wrapped line %d is %d columns, over the 12 asked for: %q", i, l.Width(), l.Text())
		}
	}
	// The styled span must still be red on the continuation row.
	last := got[len(got)-1]
	if len(last) == 0 || !last[len(last)-1].Style.Equal(red) {
		t.Fatal("the style was lost across the wrap boundary")
	}
}

func TestWrapHardBreaksAWordLongerThanTheWidth(t *testing.T) {
	// A URL or a base64 blob must not overflow: overflow wraps in the terminal,
	// which desynchronises the painter's model of the screen.
	long := strings.Repeat("a", 25)
	got := Styled("x "+long, Plain).Wrap(10)
	for i, l := range got {
		if l.Width() > 10 {
			t.Fatalf("line %d is %d columns wide: %q", i, l.Width(), l.Text())
		}
	}
	joined := ""
	for _, l := range got {
		joined += l.Text()
	}
	if !strings.Contains(strings.ReplaceAll(joined, " ", ""), long) {
		t.Fatalf("hard break lost content: %q", joined)
	}
}

func TestWrapOfEmptyLineKeepsTheRow(t *testing.T) {
	// A blank line in model output is a paragraph break. Dropping it collapses
	// the transcript's structure.
	got := Styled("", Plain).Wrap(10)
	if len(got) != 1 {
		t.Fatalf("empty line wrapped to %d rows, want 1", len(got))
	}
}

func TestPainterFirstFrameThenNoOp(t *testing.T) {
	p := NewPainter(20, 3, TrueColor)
	p.Buffer().SetString(0, 0, "hello", Plain)
	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("first frame did not paint the content: %q", out.String())
	}

	// Same content again: the diff must be essentially empty. This is the whole
	// point of the layer — a redraw of an unchanged screen must not cost a
	// screenful of bytes.
	out.Reset()
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "hello") {
		t.Fatalf("an unchanged frame repainted its content: %q", out.String())
	}
	// Nothing at all, not merely little. A UI ticks to animate spinners, and a
	// per-tick trickle over ssh is the difference between a session that is
	// quiet when idle and one that is not.
	if out.Len() != 0 {
		t.Fatalf("an unchanged frame cost %d bytes: %q", out.Len(), out.String())
	}
}

func TestPainterOnlyRepaintsWhatChanged(t *testing.T) {
	p := NewPainter(60, 5, TrueColor)
	for y := 0; y < 5; y++ {
		p.Buffer().SetString(0, y, strings.Repeat("x", 60), Plain)
	}
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{})
	firstFrame := out.Len()

	p.Buffer().SetString(10, 2, "CHANGED", Plain)
	out.Reset()
	_ = p.Flush(&out, Cursor{})

	if !strings.Contains(out.String(), "CHANGED") {
		t.Fatalf("the change was not painted: %q", out.String())
	}
	if out.Len() >= firstFrame/2 {
		t.Fatalf("a seven-cell change cost %d bytes against a %d-byte full frame", out.Len(), firstFrame)
	}
	if strings.Count(out.String(), "xxxxx") > 0 {
		t.Fatalf("unchanged cells were repainted: %q", out.String())
	}
}

func TestIdleFrameWritesNothingButACursorMoveStillDoes(t *testing.T) {
	p := NewPainter(20, 3, TrueColor)
	p.Buffer().SetString(0, 0, "text", Plain)
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{X: 1, Y: 0, Visible: true})

	out.Reset()
	_ = p.Flush(&out, Cursor{X: 1, Y: 0, Visible: true})
	if out.Len() != 0 {
		t.Fatalf("an idle frame wrote %q", out.String())
	}

	// The caret moving is a change even when no cell did — that is a keystroke
	// in a composer, and skipping it would leave the caret behind the text.
	out.Reset()
	_ = p.Flush(&out, Cursor{X: 2, Y: 0, Visible: true})
	if out.Len() == 0 {
		t.Fatal("a cursor move produced no output")
	}
	if !strings.Contains(out.String(), "\x1b[1;3H") {
		t.Fatalf("the caret was not repositioned: %q", out.String())
	}
}

func TestPainterInvalidateForcesAFullRepaint(t *testing.T) {
	// After something else writes to the terminal — Ctrl+L, a resumed suspend —
	// the painter's belief about the screen is wrong, and a diff would skip
	// cells that are correct in the model and corrupt on screen.
	p := NewPainter(20, 2, TrueColor)
	p.Buffer().SetString(0, 0, "content", Plain)
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{})

	out.Reset()
	p.Invalidate()
	_ = p.Flush(&out, Cursor{})
	if !strings.Contains(out.String(), "content") {
		t.Fatalf("Invalidate did not force a repaint: %q", out.String())
	}
}

func TestPainterRepaintsFromTheHeadOfAWideRune(t *testing.T) {
	// Repainting from a continuation cell would ask the terminal to draw the
	// right half of a glyph, which it cannot do.
	p := NewPainter(6, 1, TrueColor)
	p.Buffer().SetString(0, 0, "漢字", Plain)
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{})

	// Change only the style of the continuation cell of the first wide rune.
	p.Buffer().SetCell(1, 0, Cell{R: 0, Style: Plain.With(Underline)})
	out.Reset()
	_ = p.Flush(&out, Cursor{})
	if !strings.Contains(out.String(), "漢") {
		t.Fatalf("repaint did not include the wide rune's head: %q", out.String())
	}
}

func TestPainterResetsStyleAtEndOfFrame(t *testing.T) {
	// A frame that leaves the terminal coloured bleeds into anything written
	// afterwards, including a panic trace.
	p := NewPainter(10, 1, TrueColor)
	p.Buffer().SetString(0, 0, "hi", Plain.Foreground(RGB(255, 0, 0)))
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{})
	if !strings.HasSuffix(out.String(), "\x1b[0m") {
		t.Fatalf("frame did not end with a reset: %q", out.String())
	}
}

func TestPainterCursorPlacement(t *testing.T) {
	p := NewPainter(10, 4, TrueColor)
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{X: 3, Y: 2, Visible: true})
	if !strings.Contains(out.String(), "\x1b[3;4H") {
		t.Fatalf("cursor was not placed at 1-based row 3 col 4: %q", out.String())
	}
	if !strings.HasSuffix(out.String(), "\x1b[?25h") {
		t.Fatalf("a visible cursor must be shown last: %q", out.String())
	}

	out.Reset()
	p.Invalidate()
	_ = p.Flush(&out, Cursor{})
	if strings.Contains(out.String(), "\x1b[?25h") {
		t.Fatalf("an invisible cursor must not be shown: %q", out.String())
	}
}

func TestPainterResizeForcesRepaintAndDoesNotPanic(t *testing.T) {
	p := NewPainter(20, 5, TrueColor)
	p.Buffer().SetString(0, 0, "before", Plain)
	var out bytes.Buffer
	_ = p.Flush(&out, Cursor{})

	p.Resize(4, 1)
	p.Buffer().SetString(0, 0, "after", Plain)
	out.Reset()
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatal(err)
	}
	if w, h := p.Size(); w != 4 || h != 1 {
		t.Fatalf("size after resize = %dx%d", w, h)
	}

	p.Resize(0, 0)
	p.Buffer().SetString(0, 0, "nothing", Plain)
	out.Reset()
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("a zero-sized terminal must flush cleanly: %v", err)
	}
}

func TestProgressBarStaysWithinItsWidth(t *testing.T) {
	for _, f := range []float64{-1, 0, 0.01, 0.5, 0.999, 1, 2} {
		got := ProgressBar(10, f, Plain, Plain)
		if got.Width() != 10 {
			t.Fatalf("ProgressBar(10, %v) is %d columns, want exactly 10", f, got.Width())
		}
	}
}

func TestGradientLineKeepsTheText(t *testing.T) {
	got := GradientLine("manvi", Hex("#ff0000"), Hex("#0000ff"), 0)
	if got.Text() != "manvi" {
		t.Fatalf("gradient changed the text: %q", got.Text())
	}
	first, _, _, _ := got[0].Style.Fg.RGBA()
	last, _, _, _ := got[len(got)-1].Style.Fg.RGBA()
	if first <= last {
		t.Fatal("the gradient did not sweep from red toward blue")
	}
}

func TestBoxTooSmallDrawsNoFrame(t *testing.T) {
	// A two-column box is two border glyphs with nothing between them. During a
	// resize the layout can briefly ask for one.
	b := NewBuffer(2, 1)
	inner := Box{Border: Rounded, Style: Plain}.Draw(b, Rect{0, 0, 2, 1})
	if inner.W != 2 || inner.H != 1 {
		t.Fatalf("a too-small box returned interior %+v; it should yield the whole rect", inner)
	}
}

func TestBoxDrawsFrameAndTitle(t *testing.T) {
	b := NewBuffer(20, 4)
	inner := Box{Border: Rounded, Style: Plain, Title: Styled("scope", Plain)}.Draw(b, Rect{0, 0, 20, 4})
	if inner != (Rect{1, 1, 18, 2}) {
		t.Fatalf("interior = %+v", inner)
	}
	if b.Cell(0, 0).R != '╭' || b.Cell(19, 3).R != '╯' {
		t.Fatal("corners are missing")
	}
	row := ""
	for x := 0; x < 20; x++ {
		row += string(b.Cell(x, 0).R)
	}
	if !strings.Contains(row, "scope") {
		t.Fatalf("title row = %q", row)
	}
}

func TestSparklineIsOneColumnPerValue(t *testing.T) {
	got := Sparkline([]float64{1, 5, 3, 9, 0}, Plain)
	if got.Width() != 5 {
		t.Fatalf("sparkline of 5 values is %d columns", got.Width())
	}
	if Sparkline(nil, Plain) != nil {
		t.Fatal("an empty series must render nothing")
	}
}
