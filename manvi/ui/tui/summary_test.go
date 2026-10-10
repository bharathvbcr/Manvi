package tui

import (
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// The folded summary is built from a bounded prefix of the first line. That is
// only allowed if no reader can tell: the truncated summary must equal the one
// built from the whole line at every width, including the boundary cases —
// trailing whitespace the trim removes, tabs, wide and zero-width runes,
// invalid bytes, and a first line that ends exactly at the width.
func TestSummaryBodyTruncatesLikeTheWholeFirstLine(t *testing.T) {
	pieces := []string{"a", "word ", "日本", "e\u0301", "\t", " ", "  ", "\n", "\xff", "\uFFFD",
		"👩\u200d👩\u200d👧", "\u200b", "x", "longwordwithoutspaces", "\u3000", "…"}
	whole := func(text string) string {
		return strings.ReplaceAll(strings.TrimSpace(firstLine(text)), "\t", " ")
	}
	render1 := func(head, body string, width int) render.Line {
		return render.Line{}.Append(head, render.Style{Attrs: render.Bold}).
			Append(body, render.Style{Attrs: render.Dim}).Truncate(width)
	}
	r := rand.New(rand.NewPCG(1, 2))
	for range 50000 {
		var b strings.Builder
		for range r.IntN(40) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		if r.IntN(3) == 0 {
			b.WriteString(strings.Repeat(" ", r.IntN(60)))
		}
		if r.IntN(5) == 0 {
			b.WriteString("\ttail")
		}
		text := b.String()
		width := 1 + r.IntN(40)
		head := []string{"", "result ", "[child] ▸ thinking "}[r.IntN(3)]

		got := render1(head, summaryBody(text, width), width)
		want := render1(head, whole(text), width)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("summary of %q at width %d with head %q:\n got %#v\nwant %#v", text, width, head, got, want)
		}
	}
}

// An error or a harness message can carry a command's whole output. They take
// the cap tool results take, and say what they left out.
func TestLongErrorsAndHarnessMessagesAreCapped(t *testing.T) {
	th := PickThemeByNameUnchecked("dark", 0, true)
	huge := strings.Repeat("stderr line\n", 50000)
	for _, kind := range []ui.Kind{ui.KindError, ui.KindHarnessMessage} {
		e := &Entry{Event: ui.Event{Kind: kind, Text: huge}}
		lines := e.Lines(th, 80)
		if len(lines) > renderedMaxLines+5 {
			t.Fatalf("%s rendered %d rows", kind, len(lines))
		}
		var last strings.Builder
		for _, sp := range lines[len(lines)-1] {
			last.WriteString(sp.Text)
		}
		if !strings.Contains(last.String(), "not rendered") {
			t.Fatalf("%s: last row %q does not say what was left out", kind, last.String())
		}
	}
}
