package ui

import (
	"strings"
	"testing"
	"unicode"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// bufferHolds reports whether the render buffer stores r anywhere in a line
// written from text.
func bufferHolds(text string, r rune) bool {
	b := render.NewBuffer(16, 1)
	b.SetString(0, 0, text, render.Plain)
	for x := 0; x < 16; x++ {
		c := b.Cell(x, 0)
		if c.R == r || strings.ContainsRune(c.Comb, r) {
			return true
		}
	}
	return false
}

// Sanitize and the render buffer must decide alike about every rune that could
// be hostile or invisible. The buffer cannot hold Sanitize's visible marker, so
// a rune Sanitize marks has to be one the buffer drops, and a rune Sanitize lets
// through has to be one the buffer draws; otherwise the two layers drift (a
// visible \u200c in Persian text, or a rune that reaches the tty unmarked and
// is stored). Checked for every format rune and separator, in a Latin, an emoji
// and a Persian context, which is what the buffer's rule keys on.
func TestEveryRuneSanitizeMarksIsDroppedByTheBufferAndEveryRuneItKeepsIsDrawn(t *testing.T) {
	contexts := []struct{ pre, post string }{
		{"a", "b"},
		{"\U0001F469", "\U0001F4BB"},
		{"می", "خواهم"},
		{"क्", "ष"},
		{"1", "2"},
		{"", "x"},
	}
	n, marked, kept := 0, 0, 0
	for r := rune(0x80); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.Cf, r) && r != '\u2028' && r != '\u2029' && !isBidiControl(r) {
			continue
		}
		n++
		for _, c := range contexts {
			in := c.pre + string(r) + c.post
			out := Sanitize(in)
			sanitizeKeeps := strings.ContainsRune(out, r)
			if r == '\u2028' || r == '\u2029' {
				// Not format runes: the buffer stores them as ordinary one-column
				// cells, so only Sanitize's marking stands between them and the
				// tty. Pinned here as marked in every context.
				if sanitizeKeeps {
					t.Errorf("Sanitize keeps the separator U+%04X in %q", r, in)
				}
				continue
			}
			switch {
			case sanitizeKeeps && !bufferHolds(in, r):
				t.Errorf("Sanitize keeps U+%04X in %q but the buffer drops it", r, in)
			case !sanitizeKeeps && bufferHolds(in, r):
				t.Errorf("Sanitize marks U+%04X in %q but the buffer draws it", r, in)
			}
			if sanitizeKeeps {
				kept++
			} else {
				marked++
			}
			if isBidiControl(r) && sanitizeKeeps {
				t.Errorf("bidi control U+%04X survived Sanitize in %q", r, in)
			}
		}
	}
	if n == 0 || marked == 0 || kept == 0 {
		t.Fatalf("the sweep tested nothing: %d runes, %d marked, %d kept", n, marked, kept)
	}
}

func TestScriptJoinersPassSanitizeAndHostileFormatRunesDoNot(t *testing.T) {
	for name, in := range map[string]string{
		"persian zwnj":      "می\u200cخواهم",
		"hindi zwj":         "क्\u200dष",
		"emoji zwj family":  "\U0001F468\u200d\U0001F469\u200d\U0001F467",
		"england flag tags": "\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F",
	} {
		if got := Sanitize(in); got != in {
			t.Errorf("%s: Sanitize(%q) = %q, want it unchanged", name, in, got)
		}
	}
	for name, tc := range map[string]struct{ in, want string }{
		"right-to-left override": {"a\u202eb", "a\\u202eb"},
		"line separator":         {"a\u2028b", "a\\u2028b"},
		"paragraph separator":    {"a\u2029b", "a\\u2029b"},
		"second joiner":          {"م\u200c\u200cخ", "م\u200c\\u200cخ"},
		"bare tag after letter":  {"a\U000E0041b", "a\\U000e0041b"},
	} {
		if got := Sanitize(tc.in); got != tc.want {
			t.Errorf("%s: Sanitize(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
}
