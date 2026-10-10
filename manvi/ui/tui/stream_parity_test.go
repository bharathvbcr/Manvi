package tui

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// TestStreamedReplyLaysOutAsAWholeOne holds the incremental tail layout to the
// full one on the inputs a wrap is most likely to get wrong: double-width runes
// and emoji, combining marks, escape sequences that arrive whole, a line far
// wider than the pane, fences opened and closed across deltas, a multi-byte
// rune split between two deltas, and a resize between any two deltas. After
// every delta the frame must equal, cell for cell and style for style, what a
// transcript that renders the same text in one pass draws.
func TestStreamedReplyLaysOutAsAWholeOne(t *testing.T) {
	deltas := []string{
		"plain words ", "宽字符混合ですよ", "😀👩‍👩‍👧 ", "é́ ", "\x1b[31mred\x1b[0m ",
		"\x1b]0;title\x07", "\n", "\r\n", "\t tabbed ", strings.Repeat("w", 5000), strings.Repeat("界", 700),
		"```go\nx := 1\n", "```\n", "**bold", "** and `code", "` done\n", "# head\n", "- item\n", "1. one\n",
		"> quote\n", "|a|b|\n", "​‍", "a\xe7", "\x95b", "  \n", "",
	}
	themes := []Theme{Dark(), NoColorTheme()}
	for seed := int64(1); seed <= 8; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewScrollback()
		s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn"})
		th := themes[seed%2]
		w, h := 60, 20
		for step := 0; step < 150; step++ {
			s.Append(ui.Event{Kind: ui.KindText, Text: deltas[rng.Intn(len(deltas))]})
			if rng.Intn(6) == 0 {
				w, h = 8+rng.Intn(120), 3+rng.Intn(40)
			}
			if rng.Intn(3) != 0 {
				continue
			}
			ref := cloneUncached(s)
			got, want := render.NewBuffer(w, h), render.NewBuffer(w, h)
			gt := s.Draw(got, got.Bounds(), th, true)
			wt := ref.Draw(want, want.Bounds(), th, true)
			if gt != wt || !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d step %d (%dx%d): streamed layout differs from a whole one (total %d vs %d)\n got:\n%s\nwant:\n%s",
					seed, step, w, h, gt, wt, bufferString(got), bufferString(want))
			}
		}
	}
}
