package term

import (
	"strings"
	"testing"
	"unicode"
)

// The window title and the notification are written to the tty as OSC strings
// by Terminal.SetTitle and Terminal.Notify, not through render.Buffer.SetRune,
// so the buffer's Cf backstop does not cover them. A task title out of the
// store must not carry a bidi override or an invisible rune into them.
func TestAdvB28TitleSanitizeDropsBidiAndInvisibleFormatRunes(t *testing.T) {
	for name, r := range map[string]rune{
		"rlo":  0x202E,
		"lro":  0x202D,
		"rli":  0x2067,
		"pdi":  0x2069,
		"alm":  0x061C,
		"lrm":  0x200E,
		"zwsp": 0x200B,
		"bom":  0xFEFF,
		"tag":  0xE0041,
	} {
		if !unicode.Is(unicode.Cf, r) {
			t.Fatalf("%s is not Cf", name)
		}
		got := sanitizeTitle("fix " + string(r) + "gpj.exe")
		if strings.ContainsRune(got, r) {
			t.Errorf("%s: U+%04X survived sanitizeTitle: %q", name, r, got)
		}
	}
}
