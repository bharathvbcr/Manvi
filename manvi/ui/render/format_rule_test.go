package render

import (
	"testing"
	"unicode"
)

// Every format rune the table claims to know is category Cf, which is what
// formatMarkAllowed keys on; and a ZWJ after a letter is not an emoji.
func TestFormatMarkRule(t *testing.T) {
	for _, r := range []rune{0x202E, 0x2066, 0x200B, 0xFEFF, 0xE0041, 0x200D} {
		if !unicode.Is(unicode.Cf, r) {
			t.Fatalf("U+%04X is not Cf", r)
		}
	}
	for _, base := range []rune{'a', 'é', '5', ' ', 'क', 0x0301} {
		if formatMarkAllowed(0x200D, base, "") {
			t.Fatalf("ZWJ allowed after %q", base)
		}
	}
	if !formatMarkAllowed(0x200D, 0x1F468, "") {
		t.Fatal("ZWJ refused after an emoji")
	}
}
