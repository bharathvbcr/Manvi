package render

import (
	"strings"
	"testing"
	"unicode"
)

// Buffer.SetCell stores a Cell the caller built, so it must hold the same
// format rule SetRune does: no caller outside the painter uses it today, but
// the painter writes Comb to the tty verbatim.
func TestAdvB28SetCellCannotBypassTheFormatRule(t *testing.T) {
	for name, c := range map[string]Cell{
		"rlo in comb": {R: 'a', Comb: "\u202e"},
		"rlo as base": {R: 0x202e},
		"esc in comb": {R: 'a', Comb: "\x1b[31m"},
	} {
		got := advPaintBuf(t, 10, 1, func(b *Buffer) { b.SetCell(0, 0, c) })
		// The painter's own escapes carry ESC, so the check on a control
		// character is that what followed it did not come through either.
		for _, r := range c.Comb + string(c.R) {
			if unicode.Is(unicode.Cf, r) && strings.ContainsRune(got, r) {
				t.Errorf("%s: U+%04X reached the tty through SetCell: %q", name, r, got)
				break
			}
		}
		if strings.Contains(got, "31m") {
			t.Errorf("%s: the escape sequence in Comb reached the tty: %q", name, got)
		}
	}
}

// A mark stored on a wide rune's continuation cell is never painted, so an
// emoji sequence or a subdivision flag the buffer keeps whole reaches the tty
// as its first glyph. The painter skips continuation cells.
func TestAdvB28MarksOnAWideBaseReachTheTTY(t *testing.T) {
	for name, text := range map[string]string{
		"vs on cjk":     "\u6f22\U000e0100",
		"family":        "\U0001f468\u200d\U0001f469",
		"england flag":  "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f",
		"wide + accent": "\u6f22\u0301",
	} {
		got := advPaintBuf(t, 40, 1, func(b *Buffer) { b.SetString(0, 0, text, Plain) })
		if !strings.Contains(got, text) {
			t.Errorf("%s: input %q painted as %q", name, text, got)
		}
	}
}

// The painted bytes, not the stored cells, are what a terminal sees: every mark
// on a wide base must follow the base in order, in one run, so the cursor does
// not drift for what comes after, and a later frame that adds a mark repaints.
func TestMarksOnAWideBasePaintInOrderAndKeepTheCursor(t *testing.T) {
	for name, text := range map[string]string{
		"family":       "\U0001f468\u200d\U0001f469\u200d\U0001f467",
		"england flag": "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f",
		"vs on cjk":    "\u6f22\U000e0100",
		"cjk + accent": "\u6f22\u0301",
		"vs16 on cjk":  "\u6f22\ufe0f",
	} {
		got := advPaintBuf(t, 40, 1, func(b *Buffer) {
			if n, want := b.SetString(0, 0, text+"x", Plain), StringWidth(text+"x"); n != want {
				t.Errorf("%s: SetString consumed %d columns, StringWidth says %d", name, n, want)
			}
		})
		if !strings.Contains(got, text+"x") {
			t.Errorf("%s: base, marks and the next rune are not one run in order: %q", name, got)
		}
		if strings.Count(got, "\x1b[1;") != 1 {
			t.Errorf("%s: more than one cursor move in a single left-to-right run: %q", name, got)
		}
	}

	p := NewPainter(20, 1, NoColor)
	var out strings.Builder
	p.Buffer().SetString(0, 0, "\u6f22", Plain)
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	p.Buffer().Clear(Plain)
	p.Buffer().SetString(0, 0, "\u6f22\u0301", Plain)
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "\u6f22\u0301") {
		t.Errorf("a mark added to an already painted wide base was not repainted: %q", out.String())
	}
}

func TestStripFormatRunesKeepsTheContextualUses(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"plain":        {"abc", "abc"},
		"bidi":         {"a\u202eb", "ab"},
		"zwsp":         {"a\u200bb", "ab"},
		"soft hyphen":  {"a\u00adb", "ab"},
		"family":       {"\U0001f468\u200d\U0001f469", "\U0001f468\u200d\U0001f469"},
		"persian":      {"\u06cc\u200c\u062e", "\u06cc\u200c\u062e"},
		"zwj run":      {"\u06cc\u200d\u200d\u200d\u062e", "\u06cc\u200d\u062e"},
		"zwj digit":    {"1\u200d2", "12"},
		"flag":         {"\U0001f3f4\U000e0067\U000e0062\U000e007f", "\U0001f3f4\U000e0067\U000e0062\U000e007f"},
		"smuggled tag": {"a\U000e0041", "a"},
	} {
		if got := StripFormatRunes(tc.in); got != tc.want {
			t.Errorf("%s: StripFormatRunes(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
