package render

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// wrapQuadratic is Line.Wrap as it was before the hard break stopped measuring
// what it had left: StringWidth and TruncateWidth on the shrinking remainder at
// every cut. It is kept here and nowhere else, as the oracle the linear one is
// held to, byte for byte and span for span.
func wrapQuadratic(l Line, width int) []Line {
	if width <= 0 {
		return nil
	}
	if l.Width() <= width {
		return []Line{l}
	}

	var out []Line
	var cur Line
	curWidth := 0
	var pending Line
	pendingWidth := 0

	flushWord := func() {
		cur = append(cur, pending...)
		curWidth += pendingWidth
		pending = nil
		pendingWidth = 0
	}
	breakLine := func() {
		out = append(out, cur)
		cur = nil
		curWidth = 0
	}

	for _, span := range l {
		for _, word := range splitKeepSpace(span.Text) {
			ww := StringWidth(word)
			isSpace := strings.TrimSpace(word) == "" && word != ""

			if isSpace {
				flushWord()
				if curWidth+ww <= width {
					cur = cur.Append(word, span.Style)
					curWidth += ww
				} else if curWidth > 0 {
					breakLine()
				}
				continue
			}

			if pendingWidth+ww > width {
				flushWord()
				if curWidth > 0 {
					breakLine()
				}
				for StringWidth(word) > width {
					head := TruncateWidth(word, width, "")
					if head == "" {
						_, size := utf8.DecodeRuneInString(word)
						head = word[:size]
					}
					cur = cur.Append(head, span.Style)
					breakLine()
					word = word[len(head):]
				}
				ww = StringWidth(word)
			}

			if curWidth+pendingWidth+ww > width {
				if curWidth > 0 {
					held := pending
					heldWidth := pendingWidth
					pending, pendingWidth = nil, 0
					breakLine()
					pending, pendingWidth = held, heldWidth
				}
			}
			if word != "" {
				pending = pending.Append(word, span.Style)
				pendingWidth += ww
			}
		}
	}
	flushWord()
	if len(cur) > 0 || len(out) == 0 {
		out = append(out, cur)
	}
	for i := range out {
		out[i] = trimTrailingSpace(out[i])
	}
	return out
}

// wrapPieces are the fragments the generators join, one per class a width
// calculation can get wrong: double-width runes, ZWJ sequences and emoji,
// combining and format marks, tabs and runs of blanks, escape sequences, a
// control character, a break, and bytes that are not UTF-8 — a lead byte whose
// continuation never came, and a continuation with no lead.
var wrapPieces = []string{
	"plain", "words", "a", "x", " ", "  ", "\t", " \t ", "\n", "\x00",
	"\u5bbd\u5b57\u7b26\u6df7\u5408\u3067\u3059\u3088", "\u754c", "\u65e5\u672c\u8a9e\u306e\u30c6\u30ad\u30b9\u30c8", "\uff46\uff55\uff4c\uff4c", "\ud55c\uad6d\uc5b4",
	"\U0001F600", "\U0001F469\u200d\U0001F469\u200d\U0001F467", "\U0001F469\u200d", "\u200d", "\u200b", "\ufeff",
	"e\u0301", "\u0301\u0302", "\u00e9\u0301", "\u00f1",
	"\x1b[31m", "\x1b[0m", "\x1b]0;title\x07", "\x1b]8;;http://x\x1b\\", "\x1b",
	"a\xe7", "\x95b", "\xe7", "\xff", "\xf0\x9f", "\xe6\x97",
	"https://example.com/a/very/long/path/that/never/breaks/anywhere?q=1&r=2",
}

func randWrapLine(rng *rand.Rand) Line {
	styles := []Style{{}, {Attrs: Bold}, {Attrs: Italic}}
	var l Line
	for n := 1 + rng.Intn(4); n > 0; n-- {
		var b strings.Builder
		for k := rng.Intn(40); k > 0; k-- {
			switch rng.Intn(8) {
			case 0: // a long word: what the hard break is for
				b.WriteString(strings.Repeat(wrapPieces[rng.Intn(len(wrapPieces))], 1+rng.Intn(25)))
			default:
				b.WriteString(wrapPieces[rng.Intn(len(wrapPieces))])
			}
		}
		l = append(l, Span{Text: b.String(), Style: styles[rng.Intn(len(styles))]})
	}
	return l
}

// The linear hard break lays text out exactly as the quadratic one did: the
// same rows, the same spans, the same styles, at every width from one column
// to two hundred.
func TestWrapMatchesTheQuadraticOracle(t *testing.T) {
	cases, rows, hard := 0, 0, 0
	for seed := int64(1); seed <= 40; seed++ {
		// #nosec G404 -- seeded fixture data: reproducing a run is the point, secrecy is not.
		rng := rand.New(rand.NewSource(seed))
		for i := 0; i < 250; i++ {
			l := randWrapLine(rng)
			width := 1 + rng.Intn(200)
			if rng.Intn(3) == 0 {
				width = 1 + rng.Intn(12)
			}
			got, want := l.Wrap(width), wrapQuadratic(l, width)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d case %d width %d: rows differ\n line: %q\n  got: %v\n want: %v", seed, i, width, l.Text(), got, want)
			}
			cases++
			rows += len(want)
			for _, r := range want {
				if r.Width() >= width {
					hard++
					break
				}
			}
		}
	}
	t.Logf("%d random lines, widths 1..200, %d rows compared, %d lines with a full-width row; 0 differences", cases, rows, hard)
}

// A huge word costs time in proportion to its length. Making a word that has
// no break in it eight times longer must not multiply the time by sixty-four,
// which is what a hard break that reads the rest of the word at every cut does;
// linear is eight. Both sizes are timed several times and the fastest kept, and
// the bound sits well between the two, so a loaded machine has to be very
// unlucky to cross it from the linear side.
func TestWrapOfAHugeWordIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("timing test")
	}
	best := func(text string) time.Duration {
		l := Styled(text, Style{})
		min := time.Duration(1<<63 - 1)
		for i := 0; i < 5; i++ {
			t0 := time.Now()
			rows := l.Wrap(117)
			if d := time.Since(t0); d < min {
				min = d
			}
			var got strings.Builder
			for _, r := range rows {
				got.WriteString(r.Text())
			}
			if got.String() != text {
				t.Fatalf("wrapping a %d-byte word lost or reordered text", len(text))
			}
		}
		return min
	}
	for _, unit := range []string{"abcdefgh", "界"} {
		small := best(strings.Repeat(unit, 20_000/len(unit)))
		large := best(strings.Repeat(unit, 160_000/len(unit)))
		if ratio := float64(large) / float64(small); ratio > 24 {
			t.Errorf("%q: 8x the word took %.1fx the time (%v -> %v); a linear break is about 8x, a quadratic one about 64x", unit, ratio, small, large)
		}
	}
}
