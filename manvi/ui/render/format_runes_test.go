package render

import (
	"strings"
	"testing"
)

// TestInvisibleFormatRunesNeverReachTheTTY is the backstop for the Cf runes
// that are neither control characters nor combining marks. Every one is
// zero-width, so SetRune used to hang it off the cell to its left and the
// painter wrote it out verbatim. As in the control-character test, the
// assertion is on the painted bytes: the frame must equal the one painted
// without the rune.
func TestInvisibleFormatRunesNeverReachTheTTY(t *testing.T) {
	for name, r := range map[string]rune{
		"rlo":               0x202E,
		"lro":               0x202D,
		"rle":               0x202B,
		"lre":               0x202A,
		"pdf":               0x202C,
		"lri":               0x2066,
		"rli":               0x2067,
		"fsi":               0x2068,
		"pdi":               0x2069,
		"lrm":               0x200E,
		"rlm":               0x200F,
		"alm":               0x061C,
		"zwsp":              0x200B,
		"zwnj":              0x200C,
		"zwj after text":    0x200D,
		"word joiner":       0x2060,
		"bom":               0xFEFF,
		"tag latin a":       0xE0041,
		"tag cancel":        0xE007F,
		"invisible times":   0x2062,
		"interlinear start": 0xFFF9,
	} {
		t.Run(name, func(t *testing.T) {
			got := paint(t, 40, 2, "a"+string(r)+"b")
			want := paint(t, 40, 2, "ab")
			if got != want {
				t.Fatalf("U+%04X changed the frame\n  got: %q\n want: %q", r, got, want)
			}
			if strings.ContainsRune(got, r) {
				t.Fatalf("U+%04X reached the tty: %q", r, got)
			}
		})
	}
}

// A run of them is dropped whole, and one that opens a string has no cell to
// attach to and was already dropped.
func TestAFormatRunRunsAreDroppedWhole(t *testing.T) {
	got := paint(t, 40, 2, "a\u202e\u2066\u200b\ufeff\U000e0041b")
	if want := paint(t, 40, 2, "ab"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// The legitimate uses survive: a ZWJ between pictographs and a subdivision
// flag's tag sequence stay in the cell, exactly as TestCombiningCapLeavesRealTextAlone
// requires, and a ZWJ after a variation-selector emoji too.
func TestEmojiSequencesKeepTheirFormatRunes(t *testing.T) {
	b := NewBuffer(40, 1)
	b.SetString(0, 0, "x👨\u200d👩\u200d👧y", Plain)
	// The wide rune's continuation cell carries the joiner, as for any
	// zero-width mark after a wide rune.
	if c := b.Cell(2, 0); c.Comb != "\u200d" {
		t.Fatalf("family ZWJ was dropped: cell 2 = %q+%q", c.R, c.Comb)
	}
	if c := b.Cell(4, 0); c.Comb != "\u200d" {
		t.Fatalf("second ZWJ was dropped: cell 4 = %q+%q", c.R, c.Comb)
	}

	b = NewBuffer(40, 1)
	b.SetString(0, 0, "❤\ufe0f\u200d🔥", Plain)
	if c := b.Cell(0, 0); c.Comb != "\ufe0f\u200d" {
		t.Fatalf("heart VS16 ZWJ: cell 0 = %q+%q", c.R, c.Comb)
	}

	flag := "🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"
	b = NewBuffer(40, 1)
	b.SetString(0, 0, flag+"z", Plain)
	if c := b.Cell(1, 0); c.Comb != flag[len("🏴"):] {
		t.Fatalf("flag tags were dropped: cell 1 comb = %q", c.Comb)
	}
	if c := b.Cell(2, 0); c.R != 'z' {
		t.Fatalf("cell 2 = %q", c.R)
	}

	// Tags after anything else, and after a flag followed by other marks, go.
	b = NewBuffer(40, 1)
	b.SetString(0, 0, "🏴\u0301\U000e0067", Plain)
	if c := b.Cell(1, 0); strings.ContainsRune(c.Comb, 0xE0067) {
		t.Fatalf("a tag was accepted after a non-tag mark: %q", c.Comb)
	}
}
