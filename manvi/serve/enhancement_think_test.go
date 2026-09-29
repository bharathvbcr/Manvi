package serve

import "testing"

// strings.ToLower changes byte lengths: U+212A KELVIN SIGN (3 bytes) lowers
// to ASCII 'k' (1 byte). An index found in the lowered copy and used on the
// original sliced out of range (fuzz: slice bounds out of range [11:10]) or
// cut the wrong bytes. The tags are ASCII; so is the folding.
func TestStripEnhancementThinkKeepsIndexesOnTheOriginal(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"<think>KKK</think>{\"a\":1}", "{\"a\":1}"},
		{"<THINK>x</Think>{}", "{}"},
		{"<think>İ</think>{\"k\":\"K\"}", "{\"k\":\"K\"}"},
	} {
		got, err := stripEnhancementThink(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("stripEnhancementThink(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
	if _, err := stripEnhancementThink("<think>K</thinK>{}"); err == nil {
		t.Error("a Kelvin sign is not a 'k' in a closing tag")
	}
}
