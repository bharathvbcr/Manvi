package hygiene

import (
	"fmt"
	"strings"
	"testing"
)

// Every rune a terminal or a renderer treats as structure, or that draws as
// nothing, must be shown rather than passed through.
func TestZadvW23EscapeCoversTheInvisibleAndStructuralRunes(t *testing.T) {
	ranges := []struct {
		name   string
		lo, hi rune
	}{
		{"C0 except tab and newline, part 1", 0x00, 0x08},
		{"C0 part 2", 0x0b, 0x1f},
		{"DEL and C1", 0x7f, 0x9f},
		{"soft hyphen", 0xad, 0xad},
		{"zero width and marks ZWSP..RLM", 0x200b, 0x200f},
		{"line and paragraph separators", 0x2028, 0x2029},
		{"bidi embeddings and overrides", 0x202a, 0x202e},
		{"word joiner and invisible operators", 0x2060, 0x2064},
		{"bidi isolates", 0x2066, 0x206f},
		{"Mongolian vowel separator", 0x180e, 0x180e},
		{"byte order mark", 0xfeff, 0xfeff},
		{"interlinear annotation", 0xfff9, 0xfffb},
		{"tag block", 0xe0000, 0xe007f},
		{"private use", 0xe000, 0xe0ff},
	}
	for _, r := range ranges {
		t.Run(r.name, func(t *testing.T) {
			var missed []string
			for c := r.lo; c <= r.hi; c++ {
				in := "a" + string(c) + "b"
				if got := Escape(in); got == in {
					missed = append(missed, fmt.Sprintf("U+%04X", c))
				}
			}
			if len(missed) > 0 {
				t.Errorf("passed through unescaped: %s", strings.Join(missed, " "))
			}
		})
	}
}

func TestZadvW23EscapeLeavesNoRawEscapeSequence(t *testing.T) {
	for _, in := range []string{
		"\x1b[31mred\x1b[0m", "\x1b]0;title\x07", "\x1b]8;;http://evil\x1b\\link", "\u009b31m", "\u009d0;x\u009c", "\x1bP1;2|data\x1b\\",
	} {
		got := Escape(in)
		for _, r := range got {
			if r == 0x1b || (r >= 0x80 && r <= 0x9f) || r == 0x07 {
				t.Errorf("Escape(%q) = %q still holds %U", in, got, r)
			}
		}
	}
}

// Text already escaped is not escaped again, and legitimate text is not
// changed.
func TestZadvW23EscapeIsIdempotentAndLeavesLegitimateTextAlone(t *testing.T) {
	legit := []string{
		"plain", "a\tb\nc", "日本語", "naïve café", "emoji \U0001F600", "a literal \\x1b and \\u202e in prose", "C:\\dir\\file",
	}
	for _, in := range append([]string{"", "\r\n", "a\x00b\x1b[0m\u202e\xff\ufffd"}, legit...) {
		once := Escape(in)
		if twice := Escape(once); twice != once {
			t.Errorf("not idempotent on %q: %q then %q", in, once, twice)
		}
	}
	for _, in := range legit {
		if got := Escape(in); got != in {
			t.Errorf("legitimate text changed: %q -> %q", in, got)
		}
	}
}

// Characterization, not a defect claim: legitimate uses of ZWJ and ZWNJ (emoji
// sequences, Persian and Indic shaping) are rewritten as visible escapes.
func TestZadvW23EscapeRewritesLegitimateJoiners(t *testing.T) {
	in := "\U0001F468\u200d\U0001F469\u200d\U0001F467 \u0645\u06cc\u200c\u062e\u0648\u0627\u0647\u0645"
	if Escape(in) == in {
		t.Skip("joiners now pass through")
	}
	t.Logf("characterization: %q escapes to %q", in, Escape(in))
}

func BenchmarkZadvW23EscapeHostile1MiB(b *testing.B) {
	in := strings.Repeat("a\u202eb\x1b[0m", 1<<18)
	b.SetBytes(int64(len(in)))
	for range b.N {
		_ = Escape(in)
	}
}
