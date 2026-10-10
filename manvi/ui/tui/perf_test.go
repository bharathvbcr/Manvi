package tui

import (
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// benchTranscript fills a scrollback with a mix of the entry kinds a real
// session produces, n turns of them.
func benchTranscript(n int) *Scrollback {
	s := NewScrollback()
	for i := 0; i < n; i++ {
		s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn " + itoa(i)})
		s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file", Detail: "path=src/main.go"})
		s.Append(ui.Event{Kind: ui.KindToolResult, Text: strings.Repeat("package main // line of a file\n", 12)})
		s.Append(ui.Event{Kind: ui.KindText, Text: "Here is what I found — the **parser** splits on `\\n` and\nthen wraps each line at the pane width so a long one fits.\n"})
	}
	return s
}

func BenchmarkScrollbackIdleFrameShort(b *testing.B) {
	th := Dark()
	s := benchTranscript(1)
	buf := render.NewBuffer(100, 60)
	r := buf.Bounds()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Draw(buf, r, th, true)
	}
}

func BenchmarkScrollbackIdleFrameLong(b *testing.B) {
	th := Dark()
	s := benchTranscript(200)
	buf := render.NewBuffer(100, 60)
	r := buf.Bounds()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.Draw(buf, r, th, true)
	}
}

// BenchmarkScrollbackStreamLong is a delta arriving into a long transcript and
// the frame that follows it — the steady state of a turn.
func BenchmarkScrollbackStreamLong(b *testing.B) {
	th := Dark()
	s := benchTranscript(200)
	buf := render.NewBuffer(100, 60)
	r := buf.Bounds()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%100 == 0 {
			s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec"})
		}
		s.Append(ui.Event{Kind: ui.KindText, Text: "word "})
		s.Draw(buf, r, th, true)
	}
}

// BenchmarkScrollbackStreamShort is the same with a transcript that still fits
// the pane, which is where the scrollbar decision used to lay the content out
// at two widths every frame.
func BenchmarkScrollbackStreamShort(b *testing.B) {
	th := Dark()
	buf := render.NewBuffer(100, 60)
	r := buf.Bounds()
	var s *Scrollback
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%50 == 0 {
			s = benchTranscript(1)
		}
		s.Append(ui.Event{Kind: ui.KindText, Text: "word "})
		s.Draw(buf, r, th, true)
	}
}

// BenchmarkToolResultHuge renders a 2 MiB tool result after a width change,
// which is what a resize, a fold or a theme switch costs per such entry.
func BenchmarkToolResultHuge(b *testing.B) {
	th := Dark()
	text := strings.Repeat("0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ 0123456789\n", 2<<20/80)
	e := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: text}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Lines(th, 90+i%2)
	}
}

// cloneUncached copies a transcript's content and view state with every cache
// left empty, so drawing it is what a frame looked like before any of the
// layout caching existed.
func cloneUncached(s *Scrollback) *Scrollback {
	c := &Scrollback{
		scroll: s.scroll, follow: s.follow, selected: s.selected,
		MaxEntries: s.MaxEntries, dropped: s.dropped,
		view: s.view, viewW: s.viewW, pad: s.pad, barCol: s.barCol,
		searchQuery: s.searchQuery, matches: append([]int(nil), s.matches...), matchIdx: s.matchIdx,
	}
	for _, e := range s.entries {
		ne := &Entry{Event: e.Event, Folded: e.Folded, Foldable: e.Foldable}
		ne.Body.WriteString(e.Body.String())
		c.entries = append(c.entries, ne)
	}
	return c
}

// TestIncrementalLayoutDrawsWhatAFullRebuildDraws is the guard on the layout
// cache: across a random run of streaming, folding, searching, retiring,
// resizing and theme switches, every frame drawn from the incremental layout
// must match, cell for cell and style for style, the frame a transcript with
// no cache at all draws from the same state.
func TestIncrementalLayoutDrawsWhatAFullRebuildDraws(t *testing.T) {
	themes := []Theme{Dark(), Light(), NoColorTheme()}
	kinds := []ui.Event{
		{Kind: ui.KindNotice, Text: "a notice"},
		{Kind: ui.KindToolStart, Tool: "read_file", Detail: "path=x.go"},
		{Kind: ui.KindToolResult, Text: "one\ntwo\nthree"},
		{Kind: ui.KindToolResult, Text: strings.Repeat("long result line that wraps a bit\n", 30)},
		{Kind: ui.KindReasoning, Text: "thinking about it\nfor a while"},
		{Kind: ui.KindTurnStart, Text: "turn"},
		{Kind: ui.KindText, Text: "Hello"},
	}
	deltas := []string{"word ", "more words here ", "\n", "line\n", "```go\nfunc x() {}\n```\n", "- item\n", strings.Repeat("x", 120)}

	for seed := int64(1); seed <= 12; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewScrollback()
		s.MaxEntries = 25
		th := themes[0]
		w, h := 80, 30
		for step := 0; step < 300; step++ {
			switch op := rng.Intn(100); {
			case op < 45:
				s.Append(ui.Event{Kind: ui.KindText, Text: deltas[rng.Intn(len(deltas))]})
			case op < 65:
				s.Append(kinds[rng.Intn(len(kinds))])
			case op < 72:
				s.SelectDelta(rng.Intn(5)-2, th)
				s.ToggleFold()
			case op < 75:
				s.SetFoldAll(rng.Intn(2) == 0)
			case op < 79:
				s.SetSearch([]string{"word", "result", "notice", "zzz"}[rng.Intn(4)], th)
			case op < 82:
				s.NextMatch(th)
			case op < 85:
				th = themes[rng.Intn(len(themes))]
			case op < 90:
				w, h = 20+rng.Intn(100), 3+rng.Intn(50)
			case op < 95:
				s.ScrollBy(rng.Intn(21) - 10)
			default:
				s.SelectNone()
			}

			for _, e := range s.entries {
				if e.Event.Kind != ui.KindText {
					continue
				}
				body := e.text()
				if want := strings.Contains(body, "\n") || len(body) > 200; e.Foldable != want {
					t.Fatalf("seed %d step %d: Foldable = %v for a %d-byte body, the whole-body rule says %v",
						seed, step, e.Foldable, len(body), want)
				}
			}

			ref := cloneUncached(s)
			got, want := render.NewBuffer(w, h), render.NewBuffer(w, h)
			gotTotal := s.Draw(got, got.Bounds(), th, true)
			wantTotal := ref.Draw(want, want.Bounds(), th, true)
			if gotTotal != wantTotal || !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d step %d (%dx%d): incremental frame differs from a full rebuild (total %d vs %d)\n got:\n%s\nwant:\n%s",
					seed, step, w, h, gotTotal, wantTotal, bufferString(got), bufferString(want))
			}
			if s.scroll != ref.scroll || s.pad != ref.pad || s.barCol != ref.barCol || s.layoutTotal() != ref.layoutTotal() {
				t.Fatalf("seed %d step %d: view state differs: scroll %d/%d pad %d/%d bar %d/%d total %d/%d",
					seed, step, s.scroll, ref.scroll, s.pad, ref.pad, s.barCol, ref.barCol, s.layoutTotal(), ref.layoutTotal())
			}
		}
	}
}

// TestAnIdleFrameRendersNothing: a transcript shorter than the pane is laid out
// at two widths every frame, to decide that it needs no scrollbar. With one
// cached layout those two widths evicted each other, and every entry was
// rendered twice per frame — at ten frames a second while nothing happened.
func TestAnIdleFrameRendersNothing(t *testing.T) {
	th := Dark()
	s := benchTranscript(1)
	buf := render.NewBuffer(100, 60)
	s.Draw(buf, buf.Bounds(), th, true)
	if s.barCol != -1 {
		t.Fatal("the fixture is meant to fit the pane")
	}
	before := make([]*render.Line, len(s.entries))
	for i, e := range s.entries {
		before[i] = &e.cacheLines[0]
	}
	for frame := 0; frame < 3; frame++ {
		s.Draw(buf, buf.Bounds(), th, true)
	}
	for i, e := range s.entries {
		if len(e.cacheLines) == 0 || &e.cacheLines[0] != before[i] {
			t.Errorf("entry %d (%s) was rendered again by an idle frame", i, e.Event.Kind)
		}
	}
}

// TestADeltaRendersOnlyTheEntryItChanged: streaming text changes the last
// entry and nothing else, so nothing else is laid out again for it.
func TestADeltaRendersOnlyTheEntryItChanged(t *testing.T) {
	th := Dark()
	s := benchTranscript(40)
	buf := render.NewBuffer(100, 30)
	s.Draw(buf, buf.Bounds(), th, true)
	n := len(s.entries)
	first := &s.entries[0].cacheLines[0]
	s.Append(ui.Event{Kind: ui.KindText, Text: "more"})
	s.Draw(buf, buf.Bounds(), th, true)
	if &s.entries[0].cacheLines[0] != first {
		t.Error("a delta to the last entry re-rendered the first")
	}
	if len(s.entries) != n {
		t.Fatalf("the delta should have merged into the last entry")
	}
	if got := s.entries[n-1].text(); !strings.HasSuffix(got, "more") {
		t.Fatalf("delta not appended: %q", got)
	}
}

// TestAThemeSwitchRedrawsAScrollableTranscript: the layout used to be keyed on
// width and content alone, so a transcript long enough to scroll — which never
// changes width between frames — went on drawing the old theme's rows after a
// switch until the next event arrived.
func TestAThemeSwitchRedrawsAScrollableTranscript(t *testing.T) {
	s := benchTranscript(20)
	buf := render.NewBuffer(80, 20)
	s.Draw(buf, buf.Bounds(), Dark(), true)
	light := Light()
	got := render.NewBuffer(80, 20)
	s.Draw(got, got.Bounds(), light, true)
	want := render.NewBuffer(80, 20)
	cloneUncached(s).Draw(want, want.Bounds(), light, true)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("the frame after a theme switch still carries the old theme's rows")
	}
}

// TestFoldableStreamingMatchesTheWholeBodyRule pins the incremental rule at its
// edges: a break arriving in a later delta, and the length bound crossed
// exactly.
func TestFoldableStreamingMatchesTheWholeBodyRule(t *testing.T) {
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindText, Text: "short"})
	if s.entries[0].Foldable {
		t.Fatal("a short single line has nothing to fold")
	}
	s.Append(ui.Event{Kind: ui.KindText, Text: " and\nbroken"})
	if !s.entries[0].Foldable {
		t.Fatal("a break in a later delta makes the body foldable")
	}

	s = NewScrollback()
	s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("a", 150)})
	s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("b", 50)})
	if s.entries[0].Foldable {
		t.Fatal("exactly 200 bytes is not past the bound")
	}
	s.Append(ui.Event{Kind: ui.KindText, Text: "c"})
	if !s.entries[0].Foldable {
		t.Fatal("201 bytes is past the bound")
	}
}

// bigResult is a tool result well past the render cap, with a token near its
// end that only search can reach.
func bigResult() string {
	var b strings.Builder
	for i := 0; b.Len() < 2<<20; i++ {
		b.WriteString("line ")
		b.WriteString(itoa(i))
		b.WriteString(" of a large file, long enough to look like real source text\n")
	}
	b.WriteString("NEEDLE-at-the-very-end\n")
	return b.String()
}

func TestAHugeToolResultRendersOnlyItsHead(t *testing.T) {
	th := Dark()
	text := bigResult()
	e := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: text}}
	lines := e.Lines(th, 120)
	if len(lines) > toolResultMaxLines+5 {
		t.Fatalf("rendered %d rows; the cap is %d lines", len(lines), toolResultMaxLines)
	}
	last := lines[len(lines)-1].Text()
	shown, rest := capToolResult(text)
	if !strings.Contains(last, "not rendered") || !strings.Contains(last, groupDigits(strings.Count(rest, "\n"))+" more lines") {
		t.Fatalf("the last row does not say what was left out: %q", last)
	}
	if shown+"\n"+rest != text {
		t.Fatal("the cut lost or duplicated text")
	}
	// Lines this short reach the byte budget before the line budget.
	if len(shown) > toolResultMaxBytes || len(shown) < toolResultMaxBytes-100 {
		t.Fatalf("shown %d bytes, want just under %d", len(shown), toolResultMaxBytes)
	}
	if strings.Contains(lineText(lines), "NEEDLE") {
		t.Fatal("text past the cap was rendered")
	}
	if e.Event.Text != text {
		t.Fatal("capping the render changed the entry's text")
	}
}

// TestACappedResultIsStillWholeForSearchAndCopy: the cap is a rendering
// decision. A match past it is still found, and the selected entry's text is
// still the whole result.
func TestACappedResultIsStillWholeForSearchAndCopy(t *testing.T) {
	th := Dark()
	text := bigResult()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "before"})
	s.Append(ui.Event{Kind: ui.KindToolResult, Text: text})
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "after"})
	buf := render.NewBuffer(100, 30)
	s.Draw(buf, buf.Bounds(), th, true)

	if !s.SetSearch("needle-at-the-very-end", th) || s.MatchCount() != 1 {
		t.Fatalf("search did not reach past the cap: %s", s.SearchStatus())
	}
	if s.Selected() != s.entries[1] {
		t.Fatal("the match should select the capped result")
	}
	if got := s.SelectedText(); got != text {
		t.Fatalf("copy took %d bytes of a %d-byte result", len(got), len(text))
	}
	// The jump lands on the entry's head, as it does for any tall entry, and
	// the frame stays inside the transcript.
	s.Draw(buf, buf.Bounds(), th, true)
	if s.scroll < 0 || s.scroll > maxScroll(s.Viewport(), s.layoutTotal()) {
		t.Fatalf("scroll %d outside the transcript", s.scroll)
	}
	if !strings.Contains(bufferString(buf), "line 0 of a large file") {
		t.Fatalf("the selected result's head is not on screen:\n%s", bufferString(buf))
	}
}

func TestCapToolResult(t *testing.T) {
	under := strings.Repeat("x\n", 100)
	if shown, rest := capToolResult(under); shown != under || rest != "" {
		t.Fatal("a result under the cap must render unchanged")
	}
	exact := strings.Repeat("x\n", toolResultMaxLines)
	if shown, rest := capToolResult(exact); shown != exact || rest != "" {
		t.Fatalf("a trailing break is not a line to announce (rest %q)", rest)
	}
	over := strings.Repeat("x\n", toolResultMaxLines) + "tail"
	if shown, rest := capToolResult(over); rest != "tail" || strings.Count(shown, "\n") != toolResultMaxLines-1 {
		t.Fatalf("line cap: rest %q", rest)
	}
	if got := omittedMarker("tail"); got != "… 1 more line (4 B) not rendered" {
		t.Fatalf("marker = %q", got)
	}

	// The byte cap falls back to the last break inside the budget.
	wide := strings.Repeat(strings.Repeat("y", 999)+"\n", 100)
	shown, rest := capToolResult(wide)
	if len(shown) > toolResultMaxBytes || !strings.HasSuffix(shown, "y") || shown+"\n"+rest != wide {
		t.Fatalf("byte cap: shown %d bytes ending %q", len(shown), shown[len(shown)-3:])
	}

	// One line longer than the whole budget is cut mid-line, on a rune
	// boundary.
	line := strings.Repeat("日本語", toolResultMaxBytes)
	shown, rest = capToolResult(line)
	if !utf8.ValidString(shown) || !utf8.ValidString(rest) || shown+rest != line || len(shown) > toolResultMaxBytes {
		t.Fatalf("mid-line cut split a rune or lost text (%d + %d bytes)", len(shown), len(rest))
	}
	if !strings.Contains(omittedMarker(rest), "MiB") && !strings.Contains(omittedMarker(rest), "KiB") {
		t.Fatalf("marker = %q", omittedMarker(rest))
	}
	if groupDigits(1234567) != "1,234,567" || groupDigits(999) != "999" || groupDigits(1000) != "1,000" {
		t.Fatal("groupDigits")
	}
}
