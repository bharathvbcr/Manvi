package term

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

func advTitleCheck(t *testing.T, name, in, got string) {
	t.Helper()
	if !utf8.ValidString(got) {
		t.Errorf("%s: output is not valid UTF-8: %q", name, got)
	}
	for _, r := range got {
		switch {
		case r < 0x20 || (r >= 0x7f && r <= 0x9f):
			t.Errorf("%s: control U+%04X in %q", name, r, got)
		case unicode.Is(unicode.Cf, r) && r != 0x200C && r != 0x200D && !(r >= 0xE0020 && r <= 0xE007F):
			t.Errorf("%s: bidi or invisible format rune U+%04X in %q", name, r, got)
		}
	}
	if w := render.StringWidth(got); w > 120 {
		t.Errorf("%s: %d columns, bound is 120", name, w)
	}
}

func TestAdvB30SanitizeTitleHostile(t *testing.T) {
	allFormat := "\u202e\u202d\u2066\u2067\u2068\u2069\u200e\u200f\u061c\u200b\u2060\ufeff\u00ad\u0600\u0605\u200d\u200c\U000e0041\U000e007f"
	cases := []struct {
		name, in string
		want     *string
	}{
		{"only format runes", allFormat, advPtr("")},
		{"only joiners", strings.Repeat("\u200d", 50), advPtr("")},
		{"bidi just past the cut", strings.Repeat("a", 120) + "\u202e" + "bbb", advPtr(strings.Repeat("a", 120))},
		{"bidi inside shifts nothing", strings.Repeat("a", 118) + "\u202e\u202e" + "aaaa", advPtr(strings.Repeat("a", 120))},
		{"bidi after wide at the cut", strings.Repeat("a", 119) + "\u6f22\u202e", advPtr(strings.Repeat("a", 119))},
		{"bidi long tail", strings.Repeat("a", 200) + "\u202e\u2066", advPtr(strings.Repeat("a", 120))},
		{"invalid utf8", "ab\xff\xfe\xc2cd\x9c\x80ef", nil},
		{"lone C1 bytes", "\x80\x9c\x9d\x9b", nil},
		{"U+009C after ZWJ", "a\u200d\u009c\u202e;evil", nil},
		{"BEL after ZWJ", "a\u200d\x07\u202esteal", nil},
		{"ST after pictograph ZWJ", "\U0001f468\u200d\x1b\\\u202eend", nil},
		{"ESC CSI after ZWJ", "x\u200d\x1b[31m\u202ey", nil},
		{"BEL between joiners", "\u06cc\u200d\x07\u200d\x07\u200d\u062e", nil},
	}
	for _, tc := range cases {
		got := sanitizeTitle(tc.in)
		advTitleCheck(t, tc.name, tc.in, got)
		if tc.want != nil && got != *tc.want {
			t.Errorf("%s: sanitizeTitle = %q, want %q", tc.name, got, *tc.want)
		}
		if strings.ContainsRune(got, 0x9c) || strings.ContainsRune(got, 0x07) || strings.ContainsRune(got, 0x1b) {
			t.Errorf("%s: an OSC terminator or ESC survived: %q", tc.name, got)
		}
	}
}

func advPtr(s string) *string { return &s }
