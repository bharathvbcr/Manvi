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
// left empty, and replies rendered whole, so drawing it is what a frame looked
// like before any of the layout or stream caching existed.
func cloneUncached(s *Scrollback) *Scrollback {
	c := &Scrollback{
		scroll: s.scroll, follow: s.follow, selected: s.selected,
		MaxEntries: s.MaxEntries, dropped: s.dropped,
		view: s.view, viewW: s.viewW, pad: s.pad, barCol: s.barCol,
		searchQuery: s.searchQuery, matches: append([]int(nil), s.matches...), matchIdx: s.matchIdx,
	}
	for _, e := range s.entries {
		ne := &Entry{Event: e.Event, Folded: e.Folded, Foldable: e.Foldable, whole: true}
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
	if len(lines) > renderedMaxLines+5 {
		t.Fatalf("rendered %d rows; the cap is %d lines", len(lines), renderedMaxLines)
	}
	last := lines[len(lines)-1].Text()
	shown, rest := capRendered(text)
	if !strings.Contains(last, "not rendered") || !strings.Contains(last, groupDigits(strings.Count(rest, "\n"))+" more lines") {
		t.Fatalf("the last row does not say what was left out: %q", last)
	}
	if shown+"\n"+rest != text {
		t.Fatal("the cut lost or duplicated text")
	}
	// Lines this short reach the byte budget before the line budget.
	if len(shown) > renderedMaxBytes || len(shown) < renderedMaxBytes-100 {
		t.Fatalf("shown %d bytes, want just under %d", len(shown), renderedMaxBytes)
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
	if shown, rest := capRendered(under); shown != under || rest != "" {
		t.Fatal("a result under the cap must render unchanged")
	}
	exact := strings.Repeat("x\n", renderedMaxLines)
	if shown, rest := capRendered(exact); shown != exact || rest != "" {
		t.Fatalf("a trailing break is not a line to announce (rest %q)", rest)
	}
	over := strings.Repeat("x\n", renderedMaxLines) + "tail"
	if shown, rest := capRendered(over); rest != "tail" || strings.Count(shown, "\n") != renderedMaxLines-1 {
		t.Fatalf("line cap: rest %q", rest)
	}
	if got := omittedMarker("tail"); got != "… 1 more line (4 B) not rendered" {
		t.Fatalf("marker = %q", got)
	}

	// The byte cap falls back to the last break inside the budget.
	wide := strings.Repeat(strings.Repeat("y", 999)+"\n", 100)
	shown, rest := capRendered(wide)
	if len(shown) > renderedMaxBytes || !strings.HasSuffix(shown, "y") || shown+"\n"+rest != wide {
		t.Fatalf("byte cap: shown %d bytes ending %q", len(shown), shown[len(shown)-3:])
	}

	// One line longer than the whole budget is cut mid-line, on a rune
	// boundary.
	line := strings.Repeat("日本語", renderedMaxBytes)
	shown, rest = capRendered(line)
	if !utf8.ValidString(shown) || !utf8.ValidString(rest) || shown+rest != line || len(shown) > renderedMaxBytes {
		t.Fatalf("mid-line cut split a rune or lost text (%d + %d bytes)", len(shown), len(rest))
	}
	if !strings.Contains(omittedMarker(rest), "MiB") && !strings.Contains(omittedMarker(rest), "KiB") {
		t.Fatalf("marker = %q", omittedMarker(rest))
	}
	if groupDigits(1234567) != "1,234,567" || groupDigits(999) != "999" || groupDigits(1000) != "1,000" {
		t.Fatal("groupDigits")
	}
}

// replyDeltas is a long markdown reply cut into the small deltas a model
// streams it as: prose that wraps, a list, a heading and a fenced block, over
// and over.
func replyDeltas(n int) []string {
	para := "The parser splits the **input** on line breaks and then wraps each line at the pane width, so a long one fits; `code` stays inset. "
	var out []string
	for len(out) < n {
		for _, w := range strings.SplitAfter(para+para+"\n", " ") {
			out = append(out, w)
		}
		out = append(out, "- a list item with `inline` code\n", "## A heading\n", "```go\n", "func x() { return }\n", "```\n")
	}
	return out[:n]
}

// BenchmarkStreamLongReply streams one reply of 3,000 deltas — about 20 KiB
// of markdown — a delta and a frame at a time, into a pane it soon outgrows.
// One op is the whole reply, so the cost per op is the sum over a reply's
// growth, and a per-delta cost that grows with the reply shows up squared.
func BenchmarkStreamLongReply(b *testing.B) {
	benchStreamReply(b, 3000, 100, 60)
}

// BenchmarkStreamReplyShortPane is a reply that fits its pane for its whole
// length, so every frame lays the growing entry out at both widths.
func BenchmarkStreamReplyShortPane(b *testing.B) {
	benchStreamReply(b, 600, 100, 200)
}

func benchStreamReply(b *testing.B, n, w, h int) {
	th := Dark()
	buf := render.NewBuffer(w, h)
	r := buf.Bounds()
	deltas := replyDeltas(n)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := NewScrollback()
		for _, d := range deltas {
			s.Append(ui.Event{Kind: ui.KindText, Text: d})
			s.Draw(buf, r, th, true)
		}
	}
}

// BenchmarkReplyLayout is the layout half of BenchmarkStreamLongReply with
// the painting taken out: a 3,000-delta reply laid out after each delta at
// the two widths a short transcript's frame uses. "whole" renders the reply
// in one pass each time, as every delta did before the stream slots.
func BenchmarkReplyLayout(b *testing.B) {
	th := Dark()
	deltas := replyDeltas(3000)
	for _, whole := range []bool{true, false} {
		name := "stream"
		if whole {
			name = "whole"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				e := &Entry{Event: ui.Event{Kind: ui.KindText}, whole: whole}
				for _, d := range deltas {
					e.Body.WriteString(d)
					e.Invalidate()
					e.Lines(th, 99)
					e.Lines(th, 100)
				}
			}
		})
	}
}

// BenchmarkReasoningHuge is BenchmarkToolResultHuge for a reasoning entry.
func BenchmarkReasoningHuge(b *testing.B) {
	th := Dark()
	text := strings.Repeat("0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ 0123456789\n", 2<<20/80)
	e := &Entry{Event: ui.Event{Kind: ui.KindReasoning, Text: text}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.Lines(th, 90+i%2)
	}
}

// BenchmarkSetSearch is a query typed into a long transcript, one keystroke
// per op.
func BenchmarkSetSearch(b *testing.B) {
	th := Dark()
	s := benchTranscript(200)
	buf := render.NewBuffer(100, 60)
	s.Draw(buf, buf.Bounds(), th, true)
	queries := []string{"p", "pa", "par", "pars", "parse", "parser"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.SetSearch(queries[i%len(queries)], th)
	}
}

// mdPieces are the fragments a random reply is assembled from: every
// construct renderMarkdown recognises, the near misses of each, lines that
// wrap or hard-break, wide and zero-width-joined runes, and CRLF endings.
var mdPieces = []string{
	"```go\n", "```\n", "  ```  \n", "```", "```py\r\n", "func x() { return \"s\" }\n", "// a comment\n",
	"- item with **bold** and `code`\n", "* star item\n", "  - nested item\n", "-", "- ", "*",
	"# Heading\n", "## Second\n", "### Third\n", "#no-space\n", "#", "##", "# ",
	"+added line\n", "-removed line\n", "--- a/x.go\n", "+++ b/x.go\n", "@@ -1,2 +1,2 @@\n", "diff --git a/x b/x\n", "index 1234..5678\n",
	strings.Repeat("wrap me please ", 18), strings.Repeat("x", 150), "\n",
	"日本語のテキストは幅が二列です。", "👩‍💻🎉 done ", "é ",
	"line one\r\n", "\r\n", "\n\n", " ", "\t", "\ttabbed\tline\n",
	"**unclosed bold ", "`unclosed code ", "closing** ", "` ", "| a | b |\n|---|---|\n",
}

// randomReply assembles a markdown-ish reply and cuts it into deltas at
// arbitrary byte offsets — through a rune now and then, as a byte-oriented
// stream may, and a line break may land at either end of a delta or in it.
func randomReply(rng *rand.Rand, pieces int) []string {
	var b strings.Builder
	for i := 0; i < pieces; i++ {
		b.WriteString(mdPieces[rng.Intn(len(mdPieces))])
	}
	text := b.String()
	var out []string
	for len(text) > 0 {
		n := 1 + rng.Intn(24)
		if n > len(text) {
			n = len(text)
		}
		out = append(out, text[:n])
		text = text[n:]
	}
	return out
}

// TestAStreamedReplyRendersAsAWholeOne is the guard on the stream cache:
// after every delta of a random reply, at whichever widths the frame asks for,
// the entry's rows must equal those of an entry holding the same text and
// rendering it whole, through renderMarkdown alone.
func TestAStreamedReplyRendersAsAWholeOne(t *testing.T) {
	themes := []Theme{Dark(), Light(), NoColorTheme()}
	for seed := int64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewScrollback()
		ev := ui.Event{Kind: ui.KindText}
		if seed%3 == 0 {
			ev.Agent = "child-1"
		}
		if seed%5 == 0 {
			ev.Degraded = []string{"lint"}
		}
		th := themes[rng.Intn(len(themes))]
		w := 20 + rng.Intn(100)
		for step, d := range randomReply(rng, 20+rng.Intn(80)) {
			ev.Text = d
			s.Append(ev)
			e := s.entries[0]
			switch op := rng.Intn(100); {
			case op < 5:
				th = themes[rng.Intn(len(themes))]
			case op < 12:
				w = 9 + rng.Intn(130)
			case op < 15:
				e.Folded = !e.Folded
				e.Invalidate()
			}
			// A frame over a short transcript asks for two widths; a third now
			// and then evicts one of them.
			widths := []int{w - 1, w}
			if rng.Intn(8) == 0 {
				widths = append(widths, 9+rng.Intn(130))
			}
			for _, width := range widths {
				fresh := &Entry{Event: e.Event, Folded: e.Folded, Foldable: e.Foldable, whole: true}
				fresh.Body.WriteString(e.Body.String())
				want := fresh.Lines(th, width)
				got := e.Lines(th, width)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d step %d width %d: streamed rows differ from a whole render of %q\n got:\n%s\nwant:\n%s",
						seed, step, width, e.text(), lineText(got), lineText(want))
				}
			}
		}
	}
}

// TestStreamRowsMatchRenderMarkdown pins streamRows to renderMarkdown
// directly, without build around either, so a difference cannot hide in the
// gutter both share.
func TestStreamRowsMatchRenderMarkdown(t *testing.T) {
	th := Dark()
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		e := &Entry{Event: ui.Event{Kind: ui.KindText}}
		body := 8 + rng.Intn(120)
		for _, d := range randomReply(rng, 60) {
			e.Body.WriteString(d)
			got, ready := e.streamRows(th, body, th.Glyphs())
			md := renderMarkdown(e.text(), th, body, false)
			if len(got) != len(md) {
				t.Fatalf("seed %d: %d rows, renderMarkdown gives %d", seed, len(got), len(md))
			}
			for i := range md {
				want := md[i]
				if i < ready {
					want = e.finishRow(th, th.Glyphs(), body, i, want)
				}
				if !reflect.DeepEqual(got[i], want) {
					t.Fatalf("seed %d row %d (ready %d): %q, want %q", seed, i, ready, got[i].Text(), want.Text())
				}
			}
		}
	}
}

// TestADeltaReusesTheRowsOfFinishedLines: the point of the stream cache is
// that a delta does not render the reply again, so a finished line's rows are
// the same rows, not merely equal ones, after more text arrives.
func TestADeltaReusesTheRowsOfFinishedLines(t *testing.T) {
	th := Dark()
	s := NewScrollback()
	buf := render.NewBuffer(100, 30)
	s.Append(ui.Event{Kind: ui.KindText, Text: strings.Repeat("a finished line of the reply\n", 200)})
	s.Draw(buf, buf.Bounds(), th, true)
	e := s.entries[0]
	first := &e.stream[e.streamLast].rows[0][0]
	for _, d := range []string{"more ", "text on the ", "last line"} {
		s.Append(ui.Event{Kind: ui.KindText, Text: d})
		s.Draw(buf, buf.Bounds(), th, true)
	}
	sl := &e.stream[e.streamLast]
	if &sl.rows[0][0] != first {
		t.Fatal("a delta re-rendered lines that had already ended")
	}
	if len(sl.src) != 200*len("a finished line of the reply\n") {
		t.Fatalf("cached %d bytes; the unfinished line must not be cached", len(sl.src))
	}

	// Once another entry follows, the reply has stopped growing and its
	// stream slots are released.
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "next"})
	if e.stream[0].used || e.stream[1].used {
		t.Fatal("a settled reply kept its stream slots")
	}
}

// TestStreamingIntoAShortTranscriptDrawsWhatAFullRebuildDraws streams random
// replies into transcripts that start well inside the pane and grow past it,
// at many pane sizes, comparing every frame cell for cell with an uncached
// rebuild. Short transcripts are where each frame lays the growing reply out
// at two widths, and the crossing into a scrollbar is where those two widths
// stop being the frame's.
func TestStreamingIntoAShortTranscriptDrawsWhatAFullRebuildDraws(t *testing.T) {
	themes := []Theme{Dark(), Light(), NoColorTheme()}
	others := []ui.Event{
		{Kind: ui.KindToolStart, Tool: "read_file", Detail: "path=x.go"},
		{Kind: ui.KindToolResult, Text: "one\ntwo"},
		{Kind: ui.KindReasoning, Text: "thinking\nabout it"},
		{Kind: ui.KindNotice, Text: "a notice"},
		{Kind: ui.KindText, Agent: "child", Text: "from a child\n"},
	}
	short, tall := 0, 0
	for seed := int64(1); seed <= 40; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewScrollback()
		th := themes[rng.Intn(len(themes))]
		w, h := 20+rng.Intn(120), 8+rng.Intn(70)
		for n := rng.Intn(3); n > 0; n-- {
			s.Append(others[rng.Intn(len(others))])
		}
		deltas := randomReply(rng, 40+rng.Intn(60))
		for step := 0; step < len(deltas); step++ {
			s.Append(ui.Event{Kind: ui.KindText, Text: deltas[step]})
			switch op := rng.Intn(100); {
			case op < 4:
				// A new entry ends the reply; the next delta starts another.
				s.Append(others[rng.Intn(len(others))])
			case op < 8:
				w, h = 20+rng.Intn(120), 8+rng.Intn(70)
			case op < 10:
				th = themes[rng.Intn(len(themes))]
			case op < 13:
				s.SelectDelta(rng.Intn(3)-1, th)
				s.ToggleFold()
			case op < 15:
				s.SetSearch([]string{"item", "日本", "zzz"}[rng.Intn(3)], th)
			case op < 17:
				s.SelectNone()
			}

			ref := cloneUncached(s)
			got, want := render.NewBuffer(w, h), render.NewBuffer(w, h)
			gotTotal := s.Draw(got, got.Bounds(), th, true)
			wantTotal := ref.Draw(want, want.Bounds(), th, true)
			if gotTotal != wantTotal || !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d step %d (%dx%d): frame differs from a full rebuild (total %d vs %d)\n got:\n%s\nwant:\n%s",
					seed, step, w, h, gotTotal, wantTotal, bufferString(got), bufferString(want))
			}
			if s.scroll != ref.scroll || s.pad != ref.pad || s.barCol != ref.barCol || s.layoutTotal() != ref.layoutTotal() {
				t.Fatalf("seed %d step %d: view state differs", seed, step)
			}
			if s.barCol < 0 {
				short++
			} else {
				tall++
			}
		}
	}
	// The test means nothing unless both sides of the scrollbar decision were
	// drawn, plenty of times each.
	t.Logf("%d frames without a scrollbar, %d with one", short, tall)
	if short < 500 || tall < 500 {
		t.Fatalf("frames without a scrollbar %d, with one %d; the fixture should give plenty of both", short, tall)
	}
}

// TestAHugeReasoningRendersOnlyItsHead: reasoning is capped as a tool result
// is, unfolded or not, and stays whole for search and copy.
func TestAHugeReasoningRendersOnlyItsHead(t *testing.T) {
	th := Dark()
	text := bigResult()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: text})
	e := s.entries[0]
	if !e.Foldable {
		t.Fatal("a multi-line reasoning entry is foldable")
	}
	for _, folded := range []bool{false, true} {
		e.Folded = folded
		e.Invalidate()
		lines := e.Lines(th, 120)
		if len(lines) > renderedMaxLines+5 {
			t.Fatalf("folded=%v: rendered %d rows; the cap is %d lines", folded, len(lines), renderedMaxLines)
		}
		if strings.Contains(lineText(lines), "NEEDLE") {
			t.Fatalf("folded=%v: text past the cap was rendered", folded)
		}
		if !folded {
			_, rest := capRendered(text)
			if last := lines[len(lines)-1].Text(); !strings.Contains(last, omittedMarker(rest)) {
				t.Fatalf("the last row does not say what was left out: %q", last)
			}
		}
	}

	// Under the cap, nothing changes and no marker appears.
	small := &Entry{Event: ui.Event{Kind: ui.KindReasoning, Text: "short\nthought"}}
	if got := lineText(small.Lines(th, 80)); strings.Contains(got, "not rendered") || !strings.Contains(got, "thought") {
		t.Fatalf("an uncapped reasoning entry rendered as %q", got)
	}

	buf := render.NewBuffer(100, 30)
	s.Draw(buf, buf.Bounds(), th, true)
	if !s.SetSearch("needle-at-the-very-end", th) || s.Selected() != e {
		t.Fatalf("search did not reach past the cap: %s", s.SearchStatus())
	}
	if got := s.SelectedText(); got != text {
		t.Fatalf("copy took %d bytes of %d", len(got), len(text))
	}
}

// TestSearchMatchesWhatLoweringEveryEntryMatches: the kept lowercase copies
// must give exactly the matches lowering every entry afresh gives, across
// replies growing between searches and text whose case mapping changes its
// length.
func TestSearchMatchesWhatLoweringEveryEntryMatches(t *testing.T) {
	th := Dark()
	words := []string{"Parser", "PARSER", "parser", "İstanbul", "STRASSE", "straße", "ΣΊΣΥΦΟΣ", "σίσυφος", "Ǆ", "ǆ", "K", "\xff", "日本", "\n", " "}
	queries := []string{"parser", "PaRsEr", "i̇stanbul", "istanbul", "straße", "σίσυφος", "ǆ", "k", "日本", "�", "zz", "er s"}
	for seed := int64(1); seed <= 20; seed++ {
		rng := rand.New(rand.NewSource(seed))
		s := NewScrollback()
		s.MaxEntries = 30
		for step := 0; step < 400; step++ {
			text := words[rng.Intn(len(words))] + words[rng.Intn(len(words))]
			kinds := []ui.Kind{ui.KindText, ui.KindText, ui.KindToolResult, ui.KindNotice, ui.KindReasoning}
			s.Append(ui.Event{Kind: kinds[rng.Intn(len(kinds))], Text: text})
			if rng.Intn(3) > 0 {
				continue
			}
			q := queries[rng.Intn(len(queries))]
			s.SetSearch(q, th)
			var want []int
			for i, e := range s.entries {
				if strings.Contains(strings.ToLower(e.text()), strings.ToLower(strings.TrimSpace(q))) {
					want = append(want, i)
				}
			}
			if !reflect.DeepEqual(s.matches, want) {
				t.Fatalf("seed %d step %d query %q: matches %v, lowering afresh gives %v", seed, step, q, s.matches, want)
			}
		}
	}
}

// TestAReplacedTextIsNotTakenForAGrownOne: the stream cache is checked against
// the text, not trusted to see only appends. An entry whose text is swapped
// for one that does not begin with what was cached renders the new text.
func TestAReplacedTextIsNotTakenForAGrownOne(t *testing.T) {
	th := Dark()
	e := &Entry{Event: ui.Event{Kind: ui.KindText, Text: "```go\nfirst block\nstill code\n"}}
	e.Lines(th, 60)
	for _, text := range []string{"plain\nnot code\n", "```go\nfirst block\n", "# heading\nprose"} {
		e.Event.Text = text
		e.Invalidate()
		want := renderMarkdown(text, th, 60-gutter, false)
		if got := e.Lines(th, 60); len(got) != len(want) || !strings.Contains(lineText(got), want[len(want)-1].Text()) {
			t.Fatalf("after replacing the text with %q the entry rendered:\n%s", text, lineText(got))
		}
		fresh := &Entry{Event: e.Event, whole: true}
		if !reflect.DeepEqual(e.Lines(th, 60), fresh.Lines(th, 60)) {
			t.Fatalf("after replacing the text with %q the rows differ from a fresh entry's", text)
		}
	}
}
