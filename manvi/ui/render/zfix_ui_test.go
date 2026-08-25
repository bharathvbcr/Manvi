package render

import (
	"bytes"
	"strings"
	"testing"
)

// paint writes text into a fresh buffer and returns the bytes the painter would
// put on the tty.
//
// Every assertion in this file is on those bytes rather than on the buffer's
// cells, because the cells were never the problem: they held 'a', '[', '3',
// '1', 'm' and one invisible combining mark, which reads as harmless in any
// inspection short of serialising the frame.
func paint(t *testing.T, w, h int, text string) string {
	t.Helper()
	p := NewPainter(w, h, NoColor)
	p.Buffer().SetString(0, 0, text, Plain)
	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return out.String()
}

// TestAControlCharacterIsNeverStoredAsACombiningMark is the backstop for
// defect 1.
//
// RuneWidth answers 0 for a control character, SetRune treated every zero-width
// rune as a combining mark and appended it to the cell on its left, and the
// painter writes a cell's combining marks to the tty verbatim. So "a\x1b[31mX"
// became the cell 'a' carrying an ESC, followed by the ordinary characters
// '[', '3', '1', 'm' — and the painter reassembled a working SGR sequence out
// of them. The seam that cleans untrusted text is the fix; this is the backstop
// for the path that forgets to use it, and it is deliberately both.
func TestAControlCharacterIsNeverStoredAsACombiningMark(t *testing.T) {
	for name, text := range map[string]string{
		"sgr":          "a\x1b[31mX",
		"clear screen": "a\x1b[2Jb",
		"osc 52":       "a\x1b]52;c;bWFsaWNl\x07b",
		"dcs":          "a\x1bPq\x1b\\b",
		"c1 csi":       "a\u009b2Jb",
		"c1 dcs":       "a\u0090qb",
		"nul":          "a\x00b",
		"del":          "a\x7fb",
		"bare cr":      "a\rb",
		"backspace":    "a\bb",
		"bell":         "a\ab",
	} {
		t.Run(name, func(t *testing.T) {
			// The painter writes its own escapes — cursor addressing, erase to
			// end of line, the SGR reset — so "does the frame contain ESC" is
			// not the question. The question is whether the frame differs at all
			// from the one painted with the control characters already gone,
			// which is a statement about the content and nothing else.
			got := paint(t, 40, 2, text)
			want := paint(t, 40, 2, stripControls(text))
			if got != want {
				t.Fatalf("a control character changed the frame\n text: %q\n  got: %q\n want: %q",
					text, got, want)
			}
			// And the payload's own sequences by name, which the painter has no
			// vocabulary for and could not have produced.
			for _, seq := range []string{"\x1b[31m", "\x1b[2J", "\x1b]52;", "\x1bP", "\x1b\\", "\x00", "\x7f", "\r"} {
				if strings.Contains(text, seq) && strings.Contains(got, seq) {
					t.Fatalf("the frame carries %q from %q\nframe: %q", seq, text, got)
				}
			}
		})
	}
}

// stripControls removes exactly what SetRune now refuses to store, so a frame
// painted from its result is the frame a correct buffer must produce.
func stripControls(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r < 0x20 || (r >= 0x7f && r < 0xa0) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// TestTheBufferStillHoldsRealCombiningMarks: the backstop must not take the
// zero-width runes that are genuinely combining with it.
//
// A decomposed "é", a variation selector, and the zero-width joiner inside an
// emoji sequence are all zero-width and all legitimate. Dropping them would
// turn a correctness fix into a rendering bug, and the difference between them
// and an ESC is exactly the one the check makes: control character or not.
func TestTheBufferStillHoldsRealCombiningMarks(t *testing.T) {
	for name, text := range map[string]string{
		"combining acute":    "é",
		"variation selector": "❤️",
		"zero width joiner":  "\U0001f468‍\U0001f4bb",
	} {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer(20, 1)
			b.SetString(0, 0, text, Plain)
			var got strings.Builder
			for x := 0; x < b.W; x++ {
				c := b.Cell(x, 0)
				if c.R != 0 {
					got.WriteRune(c.R)
				}
				got.WriteString(c.Comb)
			}
			if !strings.Contains(got.String(), text) {
				t.Fatalf("the buffer lost part of %q; it holds %q", text, got.String())
			}
			if paint(t, 20, 1, text) == "" {
				t.Fatal("nothing was painted at all")
			}
		})
	}
}

// TestAControlCharacterInTheFirstColumnIsAlsoDropped.
//
// The old code only reached the append when there was a cell to the left, so a
// leading ESC was already dropped by accident. That is not the same as being
// refused, and a layout change that shifted the string one column right would
// have quietly restored the leak.
func TestAControlCharacterInTheFirstColumnIsAlsoDropped(t *testing.T) {
	for _, x := range []int{0, 1, 5} {
		p := NewPainter(40, 1, NoColor)
		p.Buffer().SetString(x, 0, "\x1b[2J", Plain)
		var out bytes.Buffer
		if err := p.Flush(&out, Cursor{}); err != nil {
			t.Fatalf("flush: %v", err)
		}
		if strings.Contains(out.String(), "\x1b[2J") {
			t.Fatalf("an escape written at column %d survived into the frame: %q", x, out.String())
		}
	}
}

// TestSetRuneReportsNoColumnsForAControlCharacter: a dropped rune consumes no
// column, which is what keeps the caller's cursor arithmetic in step with what
// was actually drawn.
func TestSetRuneReportsNoColumnsForAControlCharacter(t *testing.T) {
	b := NewBuffer(10, 1)
	if got := b.SetRune(3, 0, 0x1b, Plain); got != 0 {
		t.Fatalf("SetRune(ESC) = %d columns, want 0", got)
	}
	if got := b.Cell(2, 0).Comb; got != "" {
		t.Fatalf("the cell to the left picked up %q", got)
	}
	if got := b.SetString(0, 0, "a\x1bb", Plain); got != 2 {
		t.Fatalf("SetString = %d columns, want 2 for two printable runes", got)
	}
}
