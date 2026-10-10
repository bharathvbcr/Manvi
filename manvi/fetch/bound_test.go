package fetch

import (
	"strings"
	"testing"
)

// The rune conversion bound used to make is the reference: cutting in place
// must produce the same text, including for bytes that are not UTF-8.
func TestBoundMatchesARuneConversion(t *testing.T) {
	reference := func(s string) string {
		runes := []rune(s)
		if len(runes) <= maxExtractedRunes {
			return s
		}
		return string(runes[:maxExtractedRunes]) +
			"\n\n[… the rest of this document was not included: it exceeds the extraction limit]"
	}
	inputs := []string{
		"",
		"short",
		strings.Repeat("a", maxExtractedRunes),
		strings.Repeat("a", maxExtractedRunes+1),
		strings.Repeat("é", maxExtractedRunes+5),
		strings.Repeat("😀", maxExtractedRunes-1) + "ab",
		strings.Repeat("a", maxExtractedRunes-2) + "\xff\xfe\xfd tail",
		"\xff" + strings.Repeat("ü", maxExtractedRunes),
	}
	for i, in := range inputs {
		if got, want := bound(in), reference(in); got != want {
			t.Errorf("input %d: bound differs from the rune conversion (len %d vs %d)", i, len(got), len(want))
		}
	}
}
