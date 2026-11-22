package ui

import (
	"strings"
	"testing"
)

// TestSanitizeMarksInvisibleFormatCharacters: format characters (Cf) and the
// Unicode line and paragraph separators draw nothing or break a line, so text
// carrying them can read differently from its bytes. Each is shown escaped.
func TestSanitizeMarksInvisibleFormatCharacters(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"zero-width space":                {"a\u200bb", "a\\u200bb"},
		"lone zero-width joiner":          {"x\u200dy", "x\\u200dy"},
		"joiner between emoji is kept":    {"\U0001F468\u200d\U0001F469", "\U0001F468\u200d\U0001F469"},
		"joiner after variation selector": {"\u2764\ufe0f\u200d\U0001F525", "\u2764\ufe0f\u200d\U0001F525"},
		"joiner after text":               {"a\u200d\U0001F469", "a\\u200d\U0001F469"},
		"soft hyphen":                     {"a\u00adb", "a\\xadb"},
		"byte order mark":                 {"\ufeffx", "\\ufeffx"},
		"tag character":                   {"a\U000e0041b", "a\\U000e0041b"},
		"line separator":                  {"a\u2028b", "a\\u2028b"},
		"paragraph separator":             {"a\u2029b", "a\\u2029b"},
	} {
		if got := Sanitize(tc.in); got != tc.want {
			t.Errorf("%s: Sanitize(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
	if got := Sanitize("plain text, ordinary punctuation"); got != "plain text, ordinary punctuation" {
		t.Errorf("ordinary text was altered: %q", got)
	}
	if strings.Contains(Sanitize("x\u200dy"), "\u200d") {
		t.Error("a zero-width joiner survived")
	}
}
