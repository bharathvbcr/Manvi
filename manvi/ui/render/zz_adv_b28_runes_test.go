package render

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Adversarial tests for b28-tuirune: format runes (Cf) and the paths to the tty.

// advPaintBuf flushes a buffer built by build through the painter.
func advPaintBuf(t *testing.T, w, h int, build func(b *Buffer)) string {
	t.Helper()
	p := NewPainter(w, h, NoColor)
	build(p.Buffer())
	var out bytes.Buffer
	if err := p.Flush(&out, Cursor{}); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return out.String()
}

func advAllCf() []rune {
	var out []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Cf, r) {
			out = append(out, r)
		}
	}
	return out
}

func advHex(rs []rune) string {
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("U+%04X", r)
	}
	return strings.Join(parts, " ")
}

// Every Cf rune, written between two letters, is absent from the frame.
func TestAdvB28EveryCfRuneStaysOutOfTheFrameThroughSetString(t *testing.T) {
	var leaked []rune
	for _, r := range advAllCf() {
		if r == 0x200C || r == 0x200D {
			continue // script joiners after a letter are kept by decision; see TestJoinersAreDroppedWhereTheyJoinNothing
		}
		got := advPaintBuf(t, 40, 2, func(b *Buffer) { b.SetString(0, 0, "a"+string(r)+"b", Plain) })
		if strings.ContainsRune(got, r) {
			leaked = append(leaked, r)
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("%d Cf runes reached the painted bytes through SetString: %s", len(leaked), advHex(leaked))
	}
}

// The same through Fill, with a letter to the left of the rect and at its edge.
func TestAdvB28EveryCfRuneStaysOutOfTheFrameThroughFill(t *testing.T) {
	var leaked []rune
	for _, r := range advAllCf() {
		if r == 0x200C || r == 0x200D {
			continue // script joiners after a letter are kept by decision
		}
		for _, x := range []int{0, 1} {
			got := advPaintBuf(t, 40, 2, func(b *Buffer) {
				b.SetString(0, 0, "a", Plain)
				b.Fill(Rect{X: x, Y: 0, W: 10, H: 1}, r, Plain)
			})
			if strings.ContainsRune(got, r) {
				leaked = append(leaked, r)
				break
			}
		}
	}
	if len(leaked) > 0 {
		t.Fatalf("%d Cf runes reached the painted bytes through Fill: %s", len(leaked), advHex(leaked))
	}
}

func TestAdvB28SoftHyphenDoesNotReachTheTTY(t *testing.T) {
	got := advPaintBuf(t, 40, 2, func(b *Buffer) { b.SetString(0, 0, "ab\u00adcd", Plain) })
	if strings.ContainsRune(got, 0xAD) {
		t.Fatalf("U+00AD (Cf, RuneWidth %d) was stored as a base rune and painted: %q", RuneWidth(0xAD), got)
	}
}

// Every bidi control, in every context the buffer can be in when it arrives.
func TestAdvB28BidiRuneNeverSurvivesAnyContext(t *testing.T) {
	bidi := []rune{0x061C, 0x200E, 0x200F, 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069}
	flag := "🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"
	contexts := map[string]string{
		"start":              "%s",
		"after ascii":        "a%s",
		"after space":        " %s",
		"after wide":         "日%s本",
		"after emoji":        "😀%s😀",
		"after emoji zwj":    "👨\u200d%s👩",
		"after zwj run":      "👨\u200d\u200d\u200d%s",
		"after combining":    "e\u0301%s",
		"after vs16":         "❤\ufe0f%s",
		"after vs16 zwj":     "❤\ufe0f\u200d%s🔥",
		"after flag":         flag + "%s",
		"after flag tag":     "🏴\U000e0067%s",
		"after flag then zw": "🏴\u200d%s",
		"after tag cancel":   "🏴\U000e007f%s\U000e0067",
		"between tags":       "🏴\U000e0067%s\U000e0062\U000e007f",
		"after digit":        "5%s",
		"after Persian":      "می%s",
		"after Hindi":        "क\u094d%s",
	}
	for name, tmpl := range contexts {
		for _, r := range bidi {
			text := strings.Replace(tmpl, "%s", string(r), 1)
			got := advPaintBuf(t, 60, 2, func(b *Buffer) { b.SetString(0, 0, text, Plain) })
			if strings.ContainsRune(got, r) {
				t.Errorf("context %q: U+%04X reached the tty: %q", name, r, got)
			}
			got = advPaintBuf(t, 60, 2, func(b *Buffer) {
				b.SetString(0, 0, strings.Replace(tmpl, "%s", "", 1), Plain)
				b.Fill(Rect{X: 0, Y: 0, W: 60, H: 1}, r, Plain)
			})
			if strings.ContainsRune(got, r) {
				t.Errorf("context %q via Fill: U+%04X reached the tty: %q", name, r, got)
			}
		}
	}
}

// The exceptions are the only Cf runes that may be stored; nothing else rides
// along in the cell.
func TestAdvB28CellMarksOnlyHoldAllowedRunes(t *testing.T) {
	text := "👨\u200d\u202e\u2066\u200b\u200d👩\u200d\u202e🏴\U000e0067\u202e\U000e0062\u200b\U000e007f\U000e0001\U000e0041x"
	b := NewBuffer(60, 1)
	b.SetString(0, 0, text, Plain)
	for x := 0; x < b.W; x++ {
		for _, m := range b.Cell(x, 0).Comb {
			if unicode.Is(unicode.Cf, m) && m != 0x200D && (m < 0xE0020 || m > 0xE007F) {
				t.Errorf("cell %d holds disallowed Cf U+%04X: %q", x, m, b.Cell(x, 0).Comb)
			}
		}
	}
}

func TestAdvB28MarksPerCellAreBounded(t *testing.T) {
	big := 2000
	cases := map[string]string{
		"flag with tags":    "🏴" + strings.Repeat("\U000e0067", big),
		"emoji zwj run":     "👨" + strings.Repeat("\u200d", big),
		"letter and vs":     "a" + strings.Repeat("\ufe0f", big),
		"letter and vs sup": "a" + strings.Repeat("\U000e0100", big),
		"combining run":     "a" + strings.Repeat("\u0301", big),
	}
	for name, text := range cases {
		b := NewBuffer(10, 1)
		b.SetString(0, 0, text, Plain)
		for x := 0; x < b.W; x++ {
			if n := utf8.RuneCountInString(b.Cell(x, 0).Comb); n > MaxCombPerCell {
				t.Errorf("%s: cell %d holds %d marks, cap %d", name, x, n, MaxCombPerCell)
			}
		}
		got := advPaintBuf(t, 10, 1, func(b *Buffer) { b.SetString(0, 0, text, Plain) })
		if n := utf8.RuneCountInString(got); n > 200 {
			t.Errorf("%s: %d runes painted for one cell", name, n)
		}
	}
}

// A subdivision flag is the black flag, up to six tag letters and the cancel
// tag. A cell should not carry more than that.
func TestAdvB28FlagCarriesOnlyAWellFormedTagSequence(t *testing.T) {
	b := NewBuffer(10, 1)
	b.SetString(0, 0, "🏴"+strings.Repeat("\U000e0067", 500), Plain)
	n := 0
	for x := 0; x < b.W; x++ {
		for _, m := range b.Cell(x, 0).Comb {
			if m >= 0xE0020 && m <= 0xE007F {
				n++
			}
		}
	}
	if n > 7 {
		t.Fatalf("a flag carries %d invisible tag characters (a subdivision flag is at most 7): %d bytes of hidden ASCII per cell", n, n*4)
	}
}

// A ZWJ after a ZWJ joins nothing.
func TestAdvB28ZWJRunIsNotStoredWhole(t *testing.T) {
	b := NewBuffer(10, 1)
	b.SetString(0, 0, "👨"+strings.Repeat("\u200d", 100), Plain)
	n := 0
	for x := 0; x < b.W; x++ {
		n += strings.Count(b.Cell(x, 0).Comb, "\u200d")
	}
	if n > 1 {
		t.Fatalf("%d consecutive ZWJ stored after one emoji", n)
	}
}

func TestAdvB28TagAfterNonFlagPictographIsDropped(t *testing.T) {
	for _, base := range []string{"😀", "❤", "👨", "a", "🏳", "🏴\u0301"} {
		got := advPaintBuf(t, 40, 1, func(b *Buffer) { b.SetString(0, 0, base+"\U000e0067\U000e0062", Plain) })
		if strings.ContainsRune(got, 0xE0067) || strings.ContainsRune(got, 0xE0062) {
			t.Errorf("tag characters after %q reached the tty: %q", base, got)
		}
	}
}

func TestAdvB28VariationSelectorsSurvive(t *testing.T) {
	for _, text := range []string{"a\ufe0f", "b\U000e0100", "❤\ufe0e", "x\u0301"} {
		got := advPaintBuf(t, 40, 1, func(b *Buffer) { b.SetString(0, 0, text, Plain) })
		if !strings.Contains(got, text) {
			t.Errorf("variation selector was dropped from %q: %q", text, got)
		}
	}
}

// Legitimate text whose meaning depends on a joiner: what is painted must keep
// it. The joiners are Cf and are dropped.
func TestAdvB28LegitimateJoinersKeepTheirMeaning(t *testing.T) {
	for name, text := range map[string]string{
		"persian zwnj":         "\u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645",
		"persian zwnj 2":       "\u06a9\u062a\u0627\u0628\u200c\u0647\u0627",
		"hindi zwj":            "\u0915\u094d\u200d\u0937",
		"hindi zwnj":           "\u0915\u094d\u200c\u0937",
		"malayalam chillu zwj": "\u0d28\u0d4d\u200d",
		"malayalam zwnj":       "\u0d15\u0d4d\u200c",
		"bengali zwnj":         "\u09b0\u200c\u09cd\u09af",
		"sinhala zwj":          "\u0dc1\u0dca\u200d\u0dbb\u0dd3",
		"rainbow flag":         "\U0001f3f3\ufe0f\u200d\U0001f308",
	} {
		got := advPaintBuf(t, 60, 1, func(b *Buffer) { b.SetString(0, 0, text, Plain) })
		if !strings.Contains(got, text) {
			t.Errorf("%s: input %q (% x) painted as %q", name, text, text, got)
		}
	}
}

// A ZWJ run after a narrow pictograph is stored on a painted cell, so unlike the
// wide-base cases it does reach the tty.
func TestAdvB28ZWJRunAfterANarrowPictographIsNotPaintedWhole(t *testing.T) {
	for _, base := range []rune{0x2192, 0x2500, 0x1F3F3} {
		if RuneWidth(base) != 1 {
			t.Fatalf("U+%04X is not a one-column base (width %d); the case proves nothing", base, RuneWidth(base))
		}
		got := advPaintBuf(t, 80, 1, func(b *Buffer) {
			b.SetString(0, 0, string(base)+strings.Repeat("\u200d", 100)+"x", Plain)
		})
		if n := strings.Count(got, "\u200d"); n > 1 {
			t.Errorf("U+%04X: %d ZWJ painted to the tty after one pictograph: %q", base, n, got)
		}
	}
}
