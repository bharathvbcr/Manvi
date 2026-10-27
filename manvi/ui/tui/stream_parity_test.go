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

// openPlainPieces and openMarkupPieces are what a long open line is grown from. The plain ones are
// the classes a wrap can get wrong — double-width and ZWJ runes, combining and
// zero-width marks, tabs and runs of blanks, escape sequences, a long unbroken
// token — and a rune is cut between deltas wherever the cut falls. The markup
// ones are what ends the cache: a marker, a break, anything that can make the
// line something other than a paragraph.
var (
	openPlainPieces = []string{
		"plain words here ", "a", "bc ", " ", "  ", "\t", " \t ",
		"宽字符混合ですよ", "界", "日本語のテキスト", "ｆｕｌｌ", "한국어",
		"😀", "👩‍👩‍👧 ", "👩\u200d", "\u200d", "\u200b", "\ufeff",
		"é", "́̂", "é́", "\xcc\x81", "\xe7\x95\x8c",
		"\x1b[31m", "\x1b[0m", "\x1b]0;title\x07", "\x1b]8;;http://x\x1b\\",
		"a\xe7", "\x95b", "\xe7", "\xff", "\xf0\x9f", "\xe6\x97",
		"https://example.com/a/very/long/path/that/never/breaks/anywhere?q=1&r=2",
	}
	openMarkupPieces = []string{
		"`", "``", "```", "`code`", "`unclosed ", "**", "**bold**", "*", "\n", "\n\n", "\r\n",
		"```go\n", " - ", "- item\n", "* item\n", "# head\n", "+added\n", "-removed\n", "@@ -1 +1 @@\n", "> quote ",
	}
	// openStarts are how a reply may begin: some settle at once, and some are
	// something else for a while — a list item, a heading, a diff, a fence.
	openStarts = []string{
		"A plain beginning of a reply, long enough. ", "plain-token-with-no-break-at-all-in-it-",
		"   indented but plain, and long enough to settle ", "- a list item that goes on and on and on ",
		"# a heading that goes on and on and on ", "+an added line that goes on and on and on ",
		"```\nfenced text that goes on and on and on and on ", "-", "  -", "--- a/x\n", "diff --git a/x b/x\n",
	}
)

// growOpenLine builds a reply of n pieces: a start, then mostly plain pieces
// with a markup one now and then, as a late marker would arrive.
func growOpenLine(rng *rand.Rand, n int, markupRate int) string {
	var b strings.Builder
	b.WriteString(openStarts[rng.Intn(len(openStarts))])
	for i := 0; i < n; i++ {
		switch {
		case markupRate > 0 && rng.Intn(markupRate) == 0:
			b.WriteString(openMarkupPieces[rng.Intn(len(openMarkupPieces))])
		case rng.Intn(10) == 0: // a long word: what the hard break is for
			b.WriteString(strings.Repeat(openPlainPieces[rng.Intn(len(openPlainPieces))], 1+rng.Intn(40)))
		default:
			b.WriteString(openPlainPieces[rng.Intn(len(openPlainPieces))])
		}
	}
	return b.String()
}

func wholeLines(e *Entry, th Theme, width int) []render.Line {
	fresh := &Entry{Event: e.Event, Folded: e.Folded, Foldable: e.Foldable, whole: true}
	fresh.Body.WriteString(e.Body.String())
	return fresh.Lines(th, width)
}

// openCached is how many rows of the open line the entry's slot for a width
// holds, and false if it has no slot for it.
func openCached(e *Entry, th Theme, width int) (int, bool) {
	body := max(width-gutter, 8)
	for i := range e.stream {
		if sl := &e.stream[i]; sl.used && sl.body == body && sl.theme == th.Name {
			return len(sl.open.rows), true
		}
	}
	return 0, false
}

// TestMDKindIsTheOriginalClassification holds the refactor that moved the
// branches of mdState.line into mdState.kind to the branches as they were:
// the same rows and the same state, over random lines that hit every
// construct, its near misses, and fences opening and closing.
func TestMDKindIsTheOriginalClassification(t *testing.T) {
	th := Dark()
	lines := 0
	for seed := int64(1); seed <= 60; seed++ {
		// #nosec G404 -- seeded fixture data: reproducing a run is the point, secrecy is not.
		rng := rand.New(rand.NewSource(seed))
		var b strings.Builder
		for i := 0; i < 150; i++ {
			if rng.Intn(3) == 0 {
				b.WriteString(openPlainPieces[rng.Intn(len(openPlainPieces))])
			} else {
				b.WriteString(mdPieces[rng.Intn(len(mdPieces))])
			}
			if rng.Intn(4) == 0 {
				b.WriteString(openMarkupPieces[rng.Intn(len(openMarkupPieces))])
			}
		}
		width := 8 + rng.Intn(120)
		var st mdState
		var or mdOracle
		for _, raw := range strings.Split(b.String(), "\n") {
			got, want := st.line(nil, raw, th, width), or.line(nil, raw, th, width)
			if !reflect.DeepEqual(got, want) || st.inCode != or.inCode || st.lang != or.lang {
				t.Fatalf("seed %d: line %q at width %d\n got: %q (code %v, lang %q)\nwant: %q (code %v, lang %q)",
					seed, raw, width, lineText(got), st.inCode, st.lang, lineText(want), or.inCode, or.lang)
			}
			lines++
		}
	}
	t.Logf("%d lines over 60 seeds: rows and state identical to the original branches", lines)
}

// TestSettledMeansForGood: a line settled calls a paragraph stays one, and
// laid out as a paragraph, whatever is added to it that is not markup — and
// is no longer settled once markup arrives. The flips are the ones a looser
// test gets wrong: a "-" that is a list item only when text follows its
// blank, and a fence that needs a third backtick.
func TestSettledMeansForGood(t *testing.T) {
	th := Dark()
	for _, tc := range []struct{ head, tail string }{
		{"   -          ", "x y"}, {"-          ", "x"}, {"          ``", "`x"}, {"  ", "- item here"},
		{"diff --git", " a/x"}, {"index", " 1234"}, {"@", "@ x"}, {"##", " x"}, {"###", " x"},
		{"+", "+x"}, {"+", "x"}, {"-", "x"}, {"-", "-"},
	} {
		var st mdState
		if st.settled(tc.head) {
			t.Errorf("%q settled, but %q makes it something else (kind %d)", tc.head, tc.head+tc.tail, st.kind(tc.head+tc.tail))
		}
	}

	var st mdState
	settled := 0
	for seed := int64(1); seed <= 40; seed++ {
		// #nosec G404 -- seeded fixture data: reproducing a run is the point, secrecy is not.
		rng := rand.New(rand.NewSource(seed))
		for i := 0; i < 400; i++ {
			head := strings.NewReplacer("\n", "", "\r", "").Replace(growOpenLine(rng, rng.Intn(8), 0))
			if !st.settled(head) {
				continue
			}
			settled++
			tail := strings.NewReplacer("\n", "", "\r", "").Replace(growOpenLine(rng, rng.Intn(6), 5))
			if k := st.kind(head + tail); k != mdParagraph {
				t.Fatalf("%q settled, but %q is kind %d", head, head+tail, k)
			}
			if got, want := st.settled(head+tail), !hasInlineMarkup(head+tail); got != want {
				t.Fatalf("%q: settled %v, markup-free %v", head+tail, got, want)
			}
			width := 8 + rng.Intn(100)
			if !hasInlineMarkup(head + tail) {
				got := st.line(nil, head+tail, th, width)
				want := render.Styled(head+tail, th.Base()).Wrap(width)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%q at %d: a settled line is not laid out as a plain wrap", head+tail, width)
				}
			}
		}
	}
	if settled == 0 {
		t.Fatal("no generated line settled; the test checked nothing")
	}
	if (&mdState{inCode: true}).settled(strings.Repeat("x", 40)) {
		t.Fatal("a line inside a fence settled")
	}
	t.Logf("%d settled lines extended and re-laid out as paragraphs", settled)
}

// TestOpenLineLayoutMatchesAWholeOne is the guard on the open-line cache:
// across random growth of one reply — wide and zero-width runes, ZWJ,
// escapes, tabs, a rune cut between deltas, markers and breaks arriving late,
// a resize between any two deltas, two widths alternating, an attributed
// entry — the rows a delta's frame gets are exactly the rows a render of the
// whole text in one pass gets, and the cache is shown to have run: it
// advanced, and it was dropped by a late marker.
func TestOpenLineLayoutMatchesAWholeOne(t *testing.T) {
	themes := []Theme{Dark(), NoColorTheme()}
	var compared, advanced, dropped, engaged int
	for seed := int64(1); seed <= 32; seed++ {
		// #nosec G404 -- seeded fixture data: reproducing a run is the point, secrecy is not.
		rng := rand.New(rand.NewSource(seed))
		th := themes[seed%2]
		markupRate := []int{0, 0, 40, 300}[seed%4]
		text := growOpenLine(rng, 700, markupRate)
		e := &Entry{Event: ui.Event{Kind: ui.KindText}}
		if seed%3 == 0 {
			e.Event.Agent = "sub-agent"
		}
		w1, w2 := 11+rng.Intn(100), 11+rng.Intn(100)
		cached := map[int]int{}
		for pos := 0; pos < len(text); {
			n := 1 + rng.Intn(40)
			if rng.Intn(30) == 0 {
				n = 200 + rng.Intn(2000)
			}
			n = min(n, len(text)-pos)
			e.Body.WriteString(text[pos : pos+n])
			pos += n
			e.Invalidate()
			switch rng.Intn(12) {
			case 0:
				w1 = 11 + rng.Intn(130)
			case 1:
				w2 = 11 + rng.Intn(130)
			case 2:
				w1 = 1 + rng.Intn(10) // below the minimum body: clamped
			}
			for _, w := range []int{w1, w2} {
				got := e.Lines(th, w)
				if rng.Intn(2) == 0 && pos < len(text) {
					continue
				}
				want := wholeLines(e, th, w)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d at byte %d/%d, width %d: incremental rows differ from a whole render (%d vs %d rows)\n got tail: %q\nwant tail: %q",
						seed, pos, len(text), w, len(got), len(want), lineText(got[max(0, len(got)-3):]), lineText(want[max(0, len(want)-3):]))
				}
				compared++
				if c, ok := openCached(e, th, w); ok {
					if c > cached[w] {
						advanced++
					}
					if c == 0 && cached[w] > 0 {
						dropped++
					}
					if c > 0 {
						engaged++
					}
					cached[w] = c
				}
			}
		}
	}
	t.Logf("%d frames compared with a whole render; cache held rows in %d, advanced %d times, dropped %d times", compared, engaged, advanced, dropped)
	if engaged == 0 || advanced == 0 || dropped == 0 {
		t.Fatalf("the cache did not run (held rows %d, advanced %d, dropped %d): the comparison proved nothing", engaged, advanced, dropped)
	}
}

// A late marker ends the cache however it arrives, including split across two
// deltas, and the rows after it are those of a whole render.
func TestLateMarkerDropsTheOpenLineCache(t *testing.T) {
	th := Dark()
	for _, tc := range []struct {
		name   string
		deltas []string
	}{
		{"backtick", []string{"`"}},
		{"bold in one delta", []string{"**"}},
		{"bold split across deltas", []string{"*", "*"}},
		{"code span", []string{" see `code` here"}},
		{"break", []string{"\n"}},
		{"break then fence", []string{"\n```go\nx := 1"}},
	} {
		e := &Entry{Event: ui.Event{Kind: ui.KindText}}
		const w = 60
		for i := 0; i < 400; i++ {
			e.Body.WriteString("plain-words-and-more-plain-words-without-end,")
			e.Invalidate()
			e.Lines(th, w)
		}
		if c, _ := openCached(e, th, w); c == 0 {
			t.Fatalf("%s: the cache never engaged on a long plain paragraph", tc.name)
		}
		for _, d := range tc.deltas {
			e.Body.WriteString(d)
			e.Invalidate()
			got, want := e.Lines(th, w), wholeLines(e, th, w)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: rows after %q differ from a whole render\n got tail: %q\nwant tail: %q", tc.name, d, lineText(got[max(0, len(got)-3):]), lineText(want[max(0, len(want)-3):]))
			}
		}
		if c, _ := openCached(e, th, w); c != 0 {
			t.Errorf("%s: %d rows still cached after the marker", tc.name, c)
		}
	}
}

// The rows of an open line that have been kept are the same rows, not equal
// ones, on the next frame, and a frame wraps a few rows' worth of text rather
// than the line: this is what the cache is for.
func TestOpenLineKeepsItsRows(t *testing.T) {
	th := Dark()
	e := &Entry{Event: ui.Event{Kind: ui.KindText}}
	const w = 80
	word := "0123456789abcdefghij"
	for i := 0; i < 3000; i++ {
		e.Body.WriteString(word)
		e.Invalidate()
		e.Lines(th, w)
	}
	sl := &e.stream[e.streamLast]
	first := &sl.open.rows[0][0]
	kept := len(sl.open.rows)
	if kept < 700 {
		t.Fatalf("a %d-byte unbroken line holds %d finished rows; expected nearly all of its %d", e.Body.Len(), kept, e.Body.Len()/(w-gutter))
	}
	for i := 0; i < 50; i++ {
		e.Body.WriteString(word)
		e.Invalidate()
		e.Lines(th, w)
	}
	if &sl.open.rows[0][0] != first {
		t.Fatal("a delta rebuilt rows that were already final")
	}
	if tail := e.Body.Len() - len(sl.src) - len(sl.open.prefix); tail > 6*w {
		t.Fatalf("a frame wraps %d bytes of an unbroken line; it should wrap a few rows", tail)
	}
}
