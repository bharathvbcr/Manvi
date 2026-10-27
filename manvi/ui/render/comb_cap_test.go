package render

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// refCells lays text out the way SetString always has for text that stays under
// the cap: a base rune in its cell, a wide one with a continuation, and every
// zero-width mark appended to the cell on its left. It is the oracle the capped
// buffer must agree with wherever the cap is not reached.
func refCells(text string, w int) []Cell {
	cells := make([]Cell, w)
	for i := range cells {
		cells[i] = Cell{R: ' '}
	}
	x := 0
	for _, r := range text {
		cw := RuneWidth(r)
		switch {
		case cw == 0:
			if x-1 >= 0 && x-1 < w {
				cells[x-1].Comb += string(r)
			}
		case x+cw > w:
			return cells
		case cw == 2:
			cells[x] = Cell{R: r}
			cells[x+1] = Cell{R: 0}
			x += 2
		default:
			cells[x] = Cell{R: r}
			x++
		}
	}
	return cells
}

// Normal text, ZWJ emoji, tag sequences, keycaps and stacked diacritics from
// real scripts all render exactly as they did before the cap.
func TestCombiningCapLeavesRealTextAlone(t *testing.T) {
	cases := map[string]string{
		"ascii":                "hello, world",
		"precomposed":          "café résumé naïve",
		"decomposed acute":     "café résumé",
		"vietnamese stacked":   "Việt Nam; kế hộ̉",
		"thai three marks":     "กิ้็ ที่นี่",
		"hebrew cantillation":  "בְְּּ֑֣ רֵאשִׁית",
		"arabic vowels":        "بِسْمِ ٱللَّهِ ٱلرَّحْمَٰنِ",
		"zwj family":           "x👨‍👩‍👧‍👦y",
		"skin tone and vs16":   "👍🏽 ❤️ 1️⃣",
		"england tag flag":     "🏴\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007fz",
		"cjk and wide":         "日本語のテキスト",
		"twenty stacked marks": "a" + strings.Repeat("́", 20) + "b",
		"thirty stacked marks": "a" + strings.Repeat("̀", 30) + "b",
		"multibyte marks":      "a" + strings.Repeat("⃐", 25) + "b", // 75 bytes, 25 marks
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			b := NewBuffer(60, 1)
			b.SetString(0, 0, text, Style{})
			want := refCells(text, 60)
			for x := range want {
				got := b.Cell(x, 0)
				if got.R != want[x].R || got.Comb != want[x].Comb {
					t.Fatalf("cell %d = %q+%q, want %q+%q", x, got.R, got.Comb, want[x].R, want[x].Comb)
				}
			}
		})
	}
}

// A cell keeps its first MaxCombPerCell marks and drops the rest; its
// neighbours are unaffected, and the count is of marks, not bytes.
func TestCombiningMarksPerCellAreCapped(t *testing.T) {
	over := strings.Repeat("́", MaxCombPerCell+25)
	b := NewBuffer(10, 1)
	b.SetString(0, 0, "a"+over+"b̀̀c", Style{})
	if got := utf8.RuneCountInString(b.Cell(0, 0).Comb); got != MaxCombPerCell {
		t.Fatalf("a cell holds %d marks, want the cap %d", got, MaxCombPerCell)
	}
	if got := b.Cell(0, 0).Comb; got != strings.Repeat("́", MaxCombPerCell) {
		t.Fatalf("the cell kept something other than its first marks: %q", got)
	}
	if got := b.Cell(1, 0); got.R != 'b' || got.Comb != "̀̀" {
		t.Fatalf("the next cell was affected by the cap: %q+%q", got.R, got.Comb)
	}
	if got := b.Cell(2, 0); got.R != 'c' || got.Comb != "" {
		t.Fatalf("cell 2 = %q+%q", got.R, got.Comb)
	}

	// Three-byte marks reach 30 bytes after ten of them; the cap is on marks.
	b2 := NewBuffer(10, 1)
	b2.SetString(0, 0, "a"+strings.Repeat("⃐", 40), Style{})
	if got := utf8.RuneCountInString(b2.Cell(0, 0).Comb); got != MaxCombPerCell {
		t.Fatalf("three-byte marks: a cell holds %d, want %d", got, MaxCombPerCell)
	}
}

// A run of marks costs a constant per mark. Counting allocations is the
// deterministic form of that: every uncapped append allocated a new string.
func TestAMarkRunDoesNotAllocatePerMark(t *testing.T) {
	text := "a" + strings.Repeat("́", 100_000)
	b := NewBuffer(80, 24)
	allocs := testing.AllocsPerRun(3, func() { b.SetString(0, 0, text, Style{}) })
	if allocs > 2*MaxCombPerCell {
		t.Fatalf("drawing 100,000 marks allocated %.0f times; the cap should stop it at about %d", allocs, MaxCombPerCell)
	}
	// And it is quick: 200,000 marks took 4.6 s before the cap.
	big := "a" + strings.Repeat("́", 200_000)
	start := time.Now()
	b.SetString(0, 0, big, Style{})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("200,000 marks took %s", took)
	}
}

func BenchmarkSetStringCombiningRun(b *testing.B) {
	for _, n := range []int{25_000, 50_000, 100_000, 200_000} {
		text := "a" + strings.Repeat("́", n)
		b.Run(itoaN(n), func(b *testing.B) {
			buf := NewBuffer(80, 24)
			for i := 0; i < b.N; i++ {
				buf.SetString(0, 0, text, Style{})
			}
		})
	}
}

func itoaN(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}
