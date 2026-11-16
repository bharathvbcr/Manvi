package tui

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// frameOf draws a transcript alone and returns its rows, right-trimmed.
func frameOf(s *Scrollback, th Theme, w, h int) []string {
	b := render.NewBuffer(w, h)
	s.Draw(b, b.Bounds(), th, false)
	rows := strings.Split(strings.TrimRight(bufferString(b), "\n"), "\n")
	for i := range rows {
		rows[i] = strings.TrimRight(rows[i], " ")
	}
	return rows
}

func unicodeDark() Theme {
	th := Dark()
	th.Unicode = true
	return th
}

// A thought is drawn as it arrives and collapsed once it has stopped: live, a
// heading over its last rows; finished, the heading and how long it took.
func TestAThoughtShowsItsTailWhileItArrivesThenFolds(t *testing.T) {
	th := unicodeDark()
	at := time.Date(2026, 11, 15, 14, 0, 0, 0, time.UTC)
	s := NewScrollback()
	for i := 0; i < 30; i++ {
		s.Append(ui.Event{Kind: ui.KindReasoning, At: at.Add(time.Duration(i) * time.Second / 2),
			Text: "step " + itoa(i) + " of the plan\n"})
	}
	e := s.Entries()[0]
	live := rowsOf(e, th, 80)
	if !strings.Contains(live[0], "Thinking") {
		t.Fatalf("a live thought is not headed as one: %q", live[0])
	}
	if len(live) != 1+thoughtTail {
		t.Fatalf("a live thought drew %d rows, want its heading and %d", len(live), thoughtTail)
	}
	if !strings.Contains(live[len(live)-1], "step 29") || strings.Contains(strings.Join(live, "\n"), "step 3 of") {
		t.Fatalf("a live thought does not show its newest rows:\n%s", strings.Join(live, "\n"))
	}

	// Anything else arriving ends it.
	s.Append(ui.Event{Kind: ui.KindText, Text: "Here is the fix."})
	done := rowsOf(e, th, 80)
	if len(done) != 1 || !strings.Contains(done[0], "Thought for 14s") {
		t.Fatalf("a finished thought did not fold to its heading and duration: %q", done)
	}
	if !e.Folded || e.live {
		t.Fatalf("a finished thought is folded=%v live=%v", e.Folded, e.live)
	}

	// Opened, it is the whole thought, not the tail.
	s.selected = 0
	s.ToggleFold()
	if open := strings.Join(rowsOf(e, th, 80), "\n"); !strings.Contains(open, "step 0 of") || !strings.Contains(open, "step 29 of") {
		t.Fatalf("an opened thought is not whole:\n%s", open)
	}
}

// An operator who opens a thought while it is arriving has said how they want
// it drawn; it finishing does not fold it under them.
func TestAThoughtTheOperatorOpenedStaysOpen(t *testing.T) {
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "one\ntwo"})
	s.selected = 0
	s.ToggleFold() // folded, by hand
	s.ToggleFold() // and opened again
	s.Append(ui.Event{Kind: ui.KindTurnEnd})
	if e := s.Entries()[0]; e.Folded {
		t.Fatal("a thought the operator opened was folded when it finished")
	}
}

// A thought with no clock on its events says so by leaving the time out, never
// by printing a zero.
func TestAThoughtWithNoClockClaimsNoDuration(t *testing.T) {
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "hm"})
	s.Append(ui.Event{Kind: ui.KindTurnEnd})
	if got := rowsOf(s.Entries()[0], unicodeDark(), 60)[0]; !strings.Contains(got, "Thought") || strings.Contains(got, " for ") {
		t.Fatalf("heading = %q", got)
	}
}

// A long thought with no line break in it lays out a bounded span each frame,
// from a start that does not move with every delta.
func TestALiveThoughtLaysOutABoundedSpan(t *testing.T) {
	long := strings.Repeat("word ", 20000) // 100 KB, one paragraph
	rows := tailRows(long, 80, render.Style{}, thoughtTail)
	if len(rows) != thoughtTail {
		t.Fatalf("%d rows, want %d", len(rows), thoughtTail)
	}
	a := tailRows(long, 80, render.Style{}, thoughtTail)[0].Text()
	b := tailRows(long+"word ", 80, render.Style{}, 2*thoughtTail)
	found := false
	for _, r := range b {
		found = found || r.Text() == a
	}
	if !found {
		t.Fatal("one more word re-wrapped the rows above it: the start of the span moved")
	}
}

// A call and its result are one row: the outcome, the call, and how much came
// back. The call's own row is not drawn a second time above it.
func TestACallAndItsResultAreOneRow(t *testing.T) {
	th := unicodeDark()
	body := ""
	for i := 1; i <= 40; i++ {
		body += "line " + itoa(i) + "\n"
	}
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file", Arguments: []byte(`{"path":"a/b.go"}`)})
	running := s.rows(th, 80)
	if len(running) != 1 || !strings.HasPrefix(running[0].line.Text(), th.Glyphs().Tool) {
		t.Fatalf("a running call is not one row under the tool mark: %d rows", len(running))
	}
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "read_file", Text: body})

	rows := s.rows(th, 80)
	if len(rows) != 1 {
		t.Fatalf("a call and its result drew %d rows", len(rows))
	}
	row := rows[0].line.Text()
	for _, want := range []string{th.Glyphs().Bullet, "Read a/b.go", "40 lines"} {
		if !strings.Contains(row, want) {
			t.Fatalf("the row %q does not say %q", row, want)
		}
	}
	if strings.Contains(row, "line 1") {
		t.Fatalf("the row samples the output it counts: %q", row)
	}
	if rows[0].entry != 1 {
		t.Fatalf("the row belongs to entry %d, not to the result", rows[0].entry)
	}

	// Opened, it is the call and everything it printed.
	s.selected = 1
	if !s.ToggleFold() {
		t.Fatal("a result cannot be opened")
	}
	open := rowsOf(s.Entries()[1], th, 80)
	if len(open) != 41 || !strings.Contains(open[0], "Read a/b.go") || !strings.Contains(open[40], "line 40") {
		t.Fatalf("an opened result drew %d rows", len(open))
	}

	// One line of output is the outcome, and is shown instead of counted.
	s2 := NewScrollback()
	s2.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec", Arguments: []byte(`{"command":"git rev-parse --short HEAD"}`)})
	s2.Append(ui.Event{Kind: ui.KindToolResult, Text: "8a7bada\n"})
	if got := s2.rows(th, 80); len(got) != 1 || !strings.Contains(got[0].line.Text(), "8a7bada") ||
		!strings.Contains(got[0].line.Text(), "git rev-parse") {
		t.Fatalf("a one-line result drew %d rows", len(got))
	}
	// Where the row has no room for it, it is counted, and the count survives a
	// call too long for the row.
	if got := s2.rows(th, 30); len(got) != 1 || !strings.HasSuffix(strings.TrimRight(got[0].line.Text(), " "), "1 line") {
		t.Fatalf("a narrow row lost the count: %q", got[0].line.Text())
	}
	// Nothing printed is said, not left as a mark beside nothing.
	s3 := NewScrollback()
	s3.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec", Arguments: []byte(`{"command":"true"}`)})
	s3.Append(ui.Event{Kind: ui.KindToolResult})
	if got := s3.rows(th, 80); len(got) != 1 || !strings.Contains(got[0].line.Text(), "no output") {
		t.Fatalf("an empty result drew %d rows", len(got))
	}
}

// A failure is not reduced to a row: the call, then the head of what it
// printed, and how much follows.
func TestAFailedCallKeepsTheHeadOfItsOutput(t *testing.T) {
	th := unicodeDark()
	body := ""
	for i := 1; i <= 40; i++ {
		body += "line " + itoa(i) + "\n"
	}
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec", Arguments: []byte(`{"command":"go test ./..."}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, Text: body, IsError: true})
	failed := rowsOf(s.Entries()[1], th, 80)
	if len(failed) != 1+previewErrLines+1 {
		t.Fatalf("a folded failure drew %d rows", len(failed))
	}
	if !strings.HasPrefix(failed[0], th.Glyphs().Block) || !strings.Contains(failed[0], "go test ./...") {
		t.Fatalf("a failure is not headed by its call: %q", failed[0])
	}
	hidden := "+" + itoa(40-previewErrLines) + " lines"
	if !strings.Contains(failed[1], "line 1") || !strings.Contains(failed[len(failed)-1], hidden) {
		t.Fatalf("a failure does not show its head and what follows: %q … %q", failed[1], failed[len(failed)-1])
	}
}

// What a failed call printed is not one more column of transcript rows: the
// call says that it failed, and its output is set in behind a rule in the
// failure colour, a step quieter than the text around it.
func TestAFailureIsAQuotedBlockUnderItsCall(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_exec_command", Arguments: []byte(`{"command":"go test ./ui/input"}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "--- FAIL: TestPaste (0.01s)\nFAIL\n"})
	rows := s.rows(th, 80)
	if len(rows) != 3 {
		t.Fatalf("a two-line failure drew %d rows", len(rows))
	}
	if got := strings.TrimSpace(rows[0].line.Text()); got != th.Glyphs().Block+"  Ran go test ./ui/input  · failed" {
		t.Fatalf("a failed call reads %q", got)
	}
	rule := th.Glyphs().VBar
	for _, r := range rows[1:] {
		text := r.line.Text()
		if !strings.HasPrefix(strings.TrimLeft(text, " "), rule+" ") {
			t.Fatalf("a row of a failure is not behind the rule: %q", text)
		}
		for _, span := range r.line {
			switch {
			case strings.Contains(span.Text, rule):
				if span.Style.Fg != th.Danger {
					t.Fatalf("the rule of a failure is %v, want the failure colour", span.Style.Fg)
				}
			case strings.Contains(span.Text, "FAIL"):
				if span.Style.Fg != th.FgMuted || span.Style.Attrs&render.Bold != 0 || span.Style.Bg != th.Bg {
					t.Fatalf("what a failure printed is drawn %+v", span.Style)
				}
			}
		}
	}
	// That it failed is said in the failure colour, and survives a command too
	// long for the row.
	var said bool
	for _, span := range rows[0].line {
		if strings.Contains(span.Text, "failed") {
			said = span.Style.Fg == th.Danger
		}
	}
	if !said {
		t.Fatal("that the call failed is not in the failure colour")
	}
	long := NewScrollback()
	long.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_exec_command", Arguments: []byte(`{"command":"` + strings.Repeat("go test ./a/very/long/package/path ", 6) + `"}`)})
	long.Append(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "FAIL\n"})
	if got := long.rows(th, 60)[0].line.Text(); !strings.HasSuffix(strings.TrimRight(got, " "), "failed") {
		t.Fatalf("a long failed command lost the word that it failed: %q", got)
	}

	// On a terminal with no colour and no Unicode the block is still set in.
	plain := NoColorTheme()
	if got := s.rows(plain, 80)[1].line.Text(); !strings.Contains(got, plain.Glyphs().VBar+" --- FAIL") {
		t.Fatalf("with no colour a failure's output is not set in: %q", got)
	}
}

// What the operator asked is on the same ground as the rest of the transcript:
// its weight and the prompt mark set it apart, not a filled band behind it.
func TestWhatWasAskedHasNoFillBehindIt(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindTurnStart, At: time.Date(2026, 11, 15, 14, 2, 0, 0, time.Local),
		Text: "why does the paste reader drop the last chunk when the terminal closes mid-paste? it only happens on large pastes"})
	var mark, bold bool
	for _, r := range s.rows(th, 80) {
		for _, span := range r.line {
			if span.Style.Bg != th.Bg {
				t.Fatalf("%q is drawn on %v, not the transcript's own ground", span.Text, span.Style.Bg)
			}
			if strings.Contains(span.Text, th.Glyphs().Prompt) {
				mark = span.Style.Fg == th.Accent
			}
			if strings.Contains(span.Text, "reader") {
				bold = span.Style.Attrs&render.Bold != 0 && span.Style.Fg == th.Fg
			}
		}
	}
	if !mark || !bold {
		t.Fatalf("the question is not marked and emphasised (mark %v, bold %v)", mark, bold)
	}
}

// An edit whose result comes after a decision on it keeps its own row, and that
// row says what the result made of it: its size once it worked, that it failed
// when it did not. A failed edit is not said to have edited anything.
func TestAnEditBehindADecisionSaysHowItCameOut(t *testing.T) {
	th := unicodeDark()
	edit := ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_patch_file",
		Arguments: []byte(`{"path":"a.go","target_content":"x\n","replacement_content":"y\nz\n"}`)}
	decided := ui.Event{Kind: ui.KindPolicy, Rule: "scope.unplanned", Severity: "soft", Text: "outside the plan"}
	rows := func(result ui.Event) []string {
		s := NewScrollback()
		s.Append(edit)
		s.Append(decided)
		if got := strings.TrimSpace(s.rows(th, 80)[0].line.Text()); got != th.Glyphs().Tool+"  Editing a.go" {
			t.Fatalf("an edit waiting on its result reads %q", got)
		}
		s.Append(result)
		var out []string
		for _, r := range s.rows(th, 80) {
			out = append(out, strings.TrimSpace(r.line.Text()))
		}
		return out
	}

	// The tool's "patched a.go" under a row that already says "Edited a.go" is
	// not drawn: the call and the decision on it are the whole of it.
	got := rows(ui.Event{Kind: ui.KindToolResult, Text: "patched a.go (4 bytes)"})
	if len(got) != 2 || got[0] != th.Glyphs().Bullet+"  Edited a.go  +2  -1" {
		t.Fatalf("an edit that worked behind a decision reads %q", got)
	}

	got = rows(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "target not found"})
	if len(got) != 3 || got[0] != th.Glyphs().Bullet+"  Editing a.go  · failed" {
		t.Fatalf("an edit that failed behind a decision reads %q", got)
	}
	if !strings.HasPrefix(got[2], th.Glyphs().Block) || !strings.Contains(got[2], "target not found") {
		t.Fatalf("the failure under it reads %q", got[2])
	}
}

// The loop logs a gate's verdict twice, as the decision and as the result it
// hands back to the model, with the same text. It is drawn once: the call, and
// the decision under it.
func TestARefusalIsSaidOnce(t *testing.T) {
	th := unicodeDark()
	const reason = "manvi/.env is a protected path"
	feed := func(result ui.Event, between ...ui.Event) (*Scrollback, []string) {
		s := NewScrollback()
		s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "go"})
		s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_write_file", Arguments: []byte(`{"path":"manvi/.env","content":"x\n"}`)})
		for _, ev := range between {
			s.Append(ev)
		}
		s.Append(result)
		var out []string
		for _, r := range s.rows(th, 90) {
			if text := strings.TrimSpace(r.line.Text()); text != "" {
				out = append(out, text)
			}
		}
		return s, out
	}
	denied := ui.Event{Kind: ui.KindPolicy, Tool: "devcouncil_write_file", Rule: "path.protected", Severity: "hard", Text: reason}

	s, got := feed(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: reason}, denied)
	all := strings.Join(got, "\n")
	if strings.Count(all, reason) != 1 {
		t.Fatalf("the refusal is said %d times:\n%s", strings.Count(all, reason), all)
	}
	if got[1] != th.Glyphs().Bullet+"  Writing manvi/.env  · failed" {
		t.Fatalf("a refused write reads %q", got[1])
	}
	if !strings.HasPrefix(got[2], th.Glyphs().Block) || !strings.Contains(got[2], "path.protected (hard)") {
		t.Fatalf("the decision under it reads %q", got[2])
	}
	if strings.Count(all, th.Glyphs().Block) != 1 {
		t.Fatalf("one refusal carries %d failure marks:\n%s", strings.Count(all, th.Glyphs().Block), all)
	}
	// The result that draws nothing cannot be selected, or found by a search.
	s.Append(ui.Event{Kind: ui.KindText, Text: "done"})
	s.selected = 4
	s.SelectDelta(-1, th)
	if s.selected != 2 {
		t.Fatalf("the selection went from the reply to entry %d, want the decision", s.selected)
	}

	// A pass the rules qualified is logged the same way, and is drawn once too.
	const wrote = "wrote manvi/a.go (2 bytes)"
	_, got = feed(ui.Event{Kind: ui.KindToolResult, Text: wrote},
		ui.Event{Kind: ui.KindPolicy, Rule: "scope.unplanned", Severity: "soft", Text: wrote, Demoted: "posture=relaxed"})
	if all := strings.Join(got, "\n"); strings.Count(all, wrote) != 1 || !strings.Contains(all, "posture=relaxed") {
		t.Fatalf("a qualified pass is not drawn once with what qualified it:\n%s", all)
	}

	// A result that says something the decision did not is drawn.
	_, got = feed(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "disk full"}, denied)
	if all := strings.Join(got, "\n"); !strings.Contains(all, "disk full") || !strings.Contains(all, reason) {
		t.Fatalf("a result that differs from the decision was dropped:\n%s", all)
	}

	// One that carries a qualification of its own is drawn whatever it says:
	// the grant it names is printed under it and nowhere else.
	_, got = feed(ui.Event{Kind: ui.KindToolResult, Text: reason, GrantID: "G-12", GrantedBy: "human", Rule: "path.protected"}, denied)
	if all := strings.Join(got, "\n"); !strings.Contains(all, "G-12") {
		t.Fatalf("a result's own grant was dropped with it:\n%s", all)
	}
}

// A call keeps its own row when its result does not come straight after it: a
// decision on the call stays under the call, and two calls running at once are
// not paired by guesswork.
func TestAResultTakesOnlyTheCallStraightBeforeIt(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec"})
	s.Append(ui.Event{Kind: ui.KindPolicy, Rule: "exec.allowlist", Severity: "warn", Text: "allowed by the allowlist"})
	s.Append(ui.Event{Kind: ui.KindToolResult, Text: "ok\n"})
	rows := s.rows(th, 80)
	if len(rows) != 3 || !strings.HasPrefix(rows[0].line.Text(), th.Glyphs().Bullet) {
		t.Fatalf("a call with a decision on it drew %d rows", len(rows))
	}
	if got := rows[1].line.Text(); !strings.Contains(got, "allowed by the allowlist") || !strings.Contains(got, "exec.allowlist (warn)") {
		t.Fatalf("the decision does not name its rule on its row: %q", got)
	}

	par := NewScrollback()
	par.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file"})
	par.Append(ui.Event{Kind: ui.KindToolStart, Tool: "grep"})
	par.Append(ui.Event{Kind: ui.KindToolResult, Tool: "read_file", Text: "a\nb\n"})
	par.Append(ui.Event{Kind: ui.KindToolResult, Tool: "grep", Text: "c\n"})
	got := par.rows(th, 80)
	if len(got) != 4 {
		t.Fatalf("two calls at once drew %d rows, want each call and each result", len(got))
	}
	// A result that names its tool says so, and one of several lines shows how
	// many without passing its first off as all of it.
	if r := got[2].line.Text(); !strings.Contains(r, "read_file") || !strings.Contains(r, "2 lines") {
		t.Fatalf("an unpaired result reads %q", r)
	}
	// Two calls of one tool open at once, and results that do not say whose
	// they are: neither result is drawn on a call's row, since the transcript
	// cannot tell which call it answers.
	same := NewScrollback()
	same.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file", Arguments: []byte(`{"path":"a.go"}`)})
	same.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file", Arguments: []byte(`{"path":"b.go"}`)})
	same.Append(ui.Event{Kind: ui.KindToolResult, Text: "package a\n"})
	same.Append(ui.Event{Kind: ui.KindToolResult, Text: "package b\n"})
	sameRows := same.rows(th, 80)
	if len(sameRows) != 4 {
		t.Fatalf("two open calls of one tool drew %d rows", len(sameRows))
	}
	for _, r := range sameRows[2:] {
		if text := r.line.Text(); strings.Contains(text, "a.go") || strings.Contains(text, "b.go") {
			t.Fatalf("a result was drawn as a call it may not answer: %q", text)
		}
	}
	// A sub-agent's result is never the answer to this session's call.
	sub := NewScrollback()
	sub.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec"})
	sub.Append(ui.Event{Kind: ui.KindToolResult, Agent: "worker-1", Text: "ok\n"})
	if len(sub.rows(th, 80)) != 2 {
		t.Fatal("a child's result took the parent's call")
	}

	// A result that names no call shows its one line, or its first with the
	// count of the rest.
	one := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: "wrote a.go\n"}, Foldable: true, Folded: true}
	if r := rowsOf(one, th, 80); len(r) != 1 || !strings.Contains(r[0], "wrote a.go") {
		t.Fatalf("a one-line result folded to %q", r)
	}
	two := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: "a\nb\nc\n"}, Foldable: true, Folded: true}
	if r := rowsOf(two, th, 80); len(r) != 1 || !strings.Contains(r[0], "+2 lines") {
		t.Fatalf("a three-line result folded to %q", r)
	}
	empty := &Entry{Event: ui.Event{Kind: ui.KindToolResult}}
	if r := rowsOf(empty, th, 80); len(r) != 1 || !strings.Contains(r[0], "(no output)") {
		t.Fatalf("an empty result drew %q", r)
	}
}

// The selection never lands on a call that has no row of its own.
func TestTheSelectionSkipsACallItsResultAbsorbed(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "go"})
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec"})
	// Selected while it runs: when the result takes the row, it takes the
	// selection with it.
	s.selected = 1
	s.Append(ui.Event{Kind: ui.KindToolResult, Text: "ok\n"})
	if s.selected != 2 {
		t.Fatalf("the selection stayed on entry %d, which is no longer drawn", s.selected)
	}
	s.Append(ui.Event{Kind: ui.KindText, Text: "done"})
	for _, c := range []struct{ from, by, want int }{{3, -1, 2}, {2, -1, 0}, {0, 1, 2}, {2, 1, 3}} {
		s.selected = c.from
		s.SelectDelta(c.by, th)
		if s.selected != c.want {
			t.Fatalf("from %d by %d landed on %d, want %d", c.from, c.by, s.selected, c.want)
		}
	}
}

// One line of megabytes is not laid out whole to show its first rows: the text
// handed to the wrapper is bounded by what the preview can show, and is cut on
// a rune boundary.
func TestAHugeOneLineResultPreviewsWithoutLayingItAllOut(t *testing.T) {
	const body = 97
	// One line of about 4 MiB, three bytes to the column. The byte budget is a
	// multiple of four, so it falls inside a rune and the cut has to back off.
	letter := string(rune(0x16A0))
	huge := strings.Repeat(letter, (4<<20)/len(letter))
	head, more, clipped := previewHead(huge, previewErrLines, body)
	if len(head) > previewErrLines*body*4 || !clipped || more != 0 {
		t.Fatalf("the head of a %d-byte line is %d bytes (clipped=%v, more=%d)", len(huge), len(head), clipped, more)
	}
	if (previewErrLines*body*4)%len(letter) == 0 {
		t.Fatal("the budget falls on a rune boundary, so this does not test the back-off")
	}
	if !utf8.ValidString(head) || !strings.HasPrefix(huge, head) {
		t.Fatal("the head was cut through a rune, or is not a prefix of the text")
	}
	e := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: huge, IsError: true}, Foldable: true, Folded: true}
	rows := rowsOf(e, Dark(), body+gutter)
	if len(rows) != previewErrLines+1 || !strings.Contains(rows[len(rows)-1], "continues") {
		t.Fatalf("%d rows, last %q", len(rows), rows[len(rows)-1])
	}
	// One that worked is counted, not shown, and not measured to find that out.
	ok := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: huge}, Foldable: true, Folded: true}
	if rows := rowsOf(ok, Dark(), body+gutter); len(rows) != 1 || !strings.Contains(rows[0], "1 line") {
		t.Fatalf("a huge line that worked drew %q", rows)
	}
	// Text within the bound is the text itself, with the lines after it counted.
	if head, more, clipped := previewHead("a\nb\nc\n", 2, body); head != "a\nb" || more != 1 || clipped {
		t.Fatalf("head %q more %d clipped %v", head, more, clipped)
	}
}

// paintedWorking draws the app and returns how the working row's indicator is
// painted: the first five cells of the row (the ribbon and the space after it,
// or the spinner, its padding and the start of the label), each as its text and,
// for a cell that is not a letter or a digit, its foreground. A change that is
// only in colour therefore counts as the indicator moving, and the label's own
// pulse, which changes every tick, does not. It is "" when no row says how to
// interrupt.
func paintedWorking(a *App) string {
	const w, h, lead = 100, 30, 5
	b := render.NewBuffer(w, h)
	a.Draw(b)
	for y := 0; y < h; y++ {
		var text strings.Builder
		var cells []render.Cell
		for x := 0; x < w; x++ {
			c := b.Cell(x, y)
			if c.R == 0 {
				continue
			}
			text.WriteRune(c.R)
			cells = append(cells, c)
		}
		if !strings.Contains(text.String(), "to interrupt") {
			continue
		}
		var paint strings.Builder
		for _, c := range cells[:min(lead, len(cells))] {
			if unicode.IsLetter(c.R) || unicode.IsDigit(c.R) {
				fmt.Fprintf(&paint, "%c|", c.R)
				continue
			}
			fmt.Fprintf(&paint, "%c%v|", c.R, c.Style.Fg)
		}
		return paint.String()
	}
	return ""
}

// A running turn keeps the frame moving: with the spinner gone from the status
// bar, the working row is what a tick has to repaint.
//
// CHANGED ASSERTION (owner-approved, 2026-11-16): this used to require two
// consecutive ticks to draw different row TEXT, which is true of a spinner that
// changes frame on every tick. The Fold M ribbon that stands in for the spinner
// moves in colour only and steps every other tick, so consecutive ticks can paint
// the same row. What a running turn owes the operator is unchanged, that the
// indicator keeps changing, so the indicator's cells (see paintedWorking) are now
// held to painting more than once, text and colours, over a short window of ticks.
// The label's own pulse is left out of the comparison so it cannot stand in for a
// frozen indicator.
func TestATickMovesTheWorkingRow(t *testing.T) {
	a, _ := newTestApp()
	a.Theme.Unicode = true
	a.Dispatch(ActionTick{})
	if a.TickMoved() {
		t.Fatal("an idle session repaints on a tick")
	}
	a.Current().SetBusy(true, "working")
	a.Dispatch(ActionTick{})
	first := paintedWorking(a)
	if first == "" {
		t.Fatal("no working row while a turn runs")
	}
	seen := map[string]bool{first: true}
	for range 4 {
		a.Dispatch(ActionTick{})
		if !a.TickMoved() {
			t.Fatal("a tick during a turn does not ask for a repaint")
		}
		seen[paintedWorking(a)] = true
	}
	if len(seen) < 2 {
		t.Fatalf("four ticks drew the same working row: %q", first)
	}
}

// A grant, a demotion and an unrun check are said under a folded entry as under
// an open one, whatever the entry is.
func TestAFoldNeverHidesAQualification(t *testing.T) {
	th := unicodeDark()
	for _, ev := range []ui.Event{
		{Kind: ui.KindToolResult, Text: strings.Repeat("out\n", 30), GrantID: "G-12", GrantedBy: "human"},
		{Kind: ui.KindText, Text: strings.Repeat("prose\n", 30), Rule: "scope.unplanned", Demoted: "dev posture"},
		{Kind: ui.KindReasoning, Text: "thinking\nmore", Weakened: []string{"grants.enabled = false"}},
	} {
		e := &Entry{Event: ev, Foldable: true, Folded: true}
		got := strings.Join(rowsOf(e, th, 90), "\n")
		for _, want := range []string{"G-12", "dev posture", "grants.enabled"} {
			carried := strings.Contains(ev.GrantID+ev.Demoted+strings.Join(ev.Weakened, ""), want)
			if carried && !strings.Contains(got, want) {
				t.Fatalf("a folded %s entry hides %q:\n%s", ev.Kind, want, got)
			}
		}
		if e.Event.GrantID != "" && (strings.HasPrefix(got, th.Glyphs().Pass) || strings.HasPrefix(got, th.Glyphs().Bullet)) {
			t.Fatalf("a granted result carries the pass mark:\n%s", got)
		}
	}
}

// Blocks are separated by a blank row; a call, its outcome and the decisions on
// it are one block.
func TestTheTranscriptIsSpacedInBlocks(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	for _, ev := range []ui.Event{
		{Kind: ui.KindTurnStart, Text: "fix it"},
		{Kind: ui.KindReasoning, Text: "hm"},
		{Kind: ui.KindText, Text: "Reading."},
		{Kind: ui.KindToolStart, Tool: "read_file"},
		{Kind: ui.KindPolicy, Text: "scope note", Rule: "scope.unplanned", Severity: "soft"},
		{Kind: ui.KindToolResult, Text: "contents"},
		{Kind: ui.KindToolStart, Tool: "exec"},
		{Kind: ui.KindToolResult, Text: "ok"},
		{Kind: ui.KindNotice, Text: "first notice"},
		{Kind: ui.KindNotice, Text: "second notice"},
	} {
		s.Append(ev)
	}
	rows := frameOf(s, th, 60, 20)
	var got []string
	for _, r := range rows[len(rows)-len(s.layout):] {
		switch {
		case r == "":
			got = append(got, "")
		default:
			got = append(got, strings.Fields(r)[len(strings.Fields(r))-1])
		}
	}
	// The last word of each row, blanks kept: enough to read the shape.
	// What was said is set apart; the work between is one run, a row to a step.
	want := []string{"it", "", "Thought", "", "Reading.", "", "read_file", "(soft)", "contents", "ok", "", "notice", "notice"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("layout:\n got %q\nwant %q\n%s", got, want, strings.Join(rows, "\n"))
	}

	// A reply that ends in a line break already leaves a blank row; the spacer
	// is not added under it.
	s2 := NewScrollback()
	s2.Append(ui.Event{Kind: ui.KindText, Text: "Reading.\n"})
	s2.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file"})
	frameOf(s2, th, 60, 10)
	if n := len(s2.layout); n != 3 {
		t.Fatalf("%d rows for a reply, its trailing break and a call; want 3", n)
	}
}

// A click lands on the entry whose row it is with spacers in the layout, and a
// spacer is not painted as part of the selection.
func TestSpacersDoNotMoveClicksOrTakeTheHighlight(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "first"})
	s.Append(ui.Event{Kind: ui.KindText, Text: "reply"})
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "second"})
	b := render.NewBuffer(40, 5)
	s.Draw(b, b.Bounds(), th, true)
	// Rows: first, blank, reply, blank, second.
	for y, want := range map[int]string{0: "first", 2: "reply", 4: "second"} {
		if !s.SelectAt(y) || s.Selected().text() != want {
			t.Fatalf("row %d selected %v, want %q", y, s.Selected(), want)
		}
	}
	s.selected = 1
	s.Draw(b, b.Bounds(), th, true)
	if b.Cell(5, 1).Style.Bg != th.Bg || b.Cell(5, 2).Style.Bg == th.Bg {
		t.Fatal("the highlight is on the spacer, or not on the entry")
	}
	// Selecting an entry reveals its own first row, not the blank above it.
	tall := NewScrollback()
	for i := 0; i < 30; i++ {
		tall.Append(ui.Event{Kind: ui.KindTurnStart, Text: "turn " + itoa(i)})
	}
	tb := render.NewBuffer(40, 6)
	tall.Draw(tb, tb.Bounds(), th, true)
	tall.selected = 10
	tall.revealSelected(th)
	if r := tall.layout[tall.scroll]; r.gap || r.entry != 10 {
		t.Fatalf("revealing entry 10 put row %+v at the top", r)
	}
}

// The working row says what the turn is doing, next to the composer, and is
// gone when the turn is.
func TestTheWorkingRowSaysWhatTheTurnIsDoing(t *testing.T) {
	a, _ := newTestApp()
	a.Theme.Unicode = true
	v := a.Current()
	has := func(want string) bool { return contains2(screen(a, 100, 30), want) }

	if has("to interrupt") {
		t.Fatal("an idle session shows a working row")
	}
	v.SetBusy(true, "working")
	for _, c := range []struct {
		ev   ui.Event
		want string
	}{
		{ui.Event{Kind: ui.KindTurnStart, Text: "go"}, "Working"},
		{ui.Event{Kind: ui.KindReasoning, Text: "hm"}, "Thinking"},
		{ui.Event{Kind: ui.KindText, Text: "so"}, "Writing"},
		{ui.Event{Kind: ui.KindToolStart, Tool: "exec"}, "Using exec"},
		{ui.Event{Kind: ui.KindToolResult, Text: "ok"}, "Working"},
	} {
		v.Apply(c.ev)
		rows := screen(a, 100, 30)
		row := ""
		for _, r := range rows {
			if strings.Contains(r, "to interrupt") {
				row = r
			}
		}
		if !strings.Contains(row, c.want) {
			t.Fatalf("after %s the working row is %q, want %q in it", c.ev.Kind, row, c.want)
		}
	}
	// It is the row directly above the composer.
	rows := screen(a, 100, 30)
	for i, r := range rows {
		if strings.Contains(r, "to interrupt") && !strings.HasPrefix(rows[i+1], "╭") {
			t.Fatalf("the working row is not on the composer:\n%s\n%s", r, rows[i+1])
		}
	}
	// A label the host set is the host's, and is shown as it is.
	v.SetBusy(true, "compacting the session")
	if !has("compacting the session") {
		t.Fatal("a host's own label was replaced")
	}
	v.SetBusy(false, "")
	if has("to interrupt") {
		t.Fatal("the working row outlived the turn")
	}
}

// The composer's frame says a send will be queued while a turn runs — the word
// the border sweep used to paint over — and nothing when it is idle.
func TestTheComposerFrameOnlyCarriesWhatChangesEnter(t *testing.T) {
	a, _ := newTestApp()
	a.Theme.Unicode = true
	if contains2(screen(a, 100, 30), "compose") {
		t.Fatal("an idle composer is labelled")
	}
	a.Current().SetBusy(true, "working")
	if !contains2(screen(a, 100, 30), "queue follow-up") {
		t.Fatal("a busy composer does not say a send is queued")
	}
	// The prompt mark sits inside the frame, and the editable area starts after it.
	v := a.Current()
	rows := screen(a, 100, 30)
	if got := []rune(rows[v.promptRect.Y]); string(got[v.promptRect.X-promptMark]) != a.Theme.Glyphs().Prompt {
		t.Fatalf("no prompt mark before the editable area: %q", rows[v.promptRect.Y])
	}
}

// The posture is never dropped, whole segments are, and the grant count
// outlasts everything else.
func TestTheStatusBarDropsWholeSegmentsInOrder(t *testing.T) {
	th := unicodeDark()
	st := Status{
		Posture: "strict", Model: "claude-opus-5-5", Provider: "anthropic", TaskID: "TASK-7",
		GitBranch: "main", ContextUsed: 5, ContextMax: 10, Steps: 4,
		InputTokens: 1200, OutputTokens: 300, Grants: 2, Sessions: 3,
	}
	last := ""
	for w := 140; w >= 6; w-- {
		b := render.NewBuffer(w, 1)
		DrawStatusBar(b, b.Bounds(), th, st, 0, nil, false)
		out := strings.TrimRight(bufferString(b), "\n")
		if w >= 9 && !strings.Contains(out, "strict") {
			t.Fatalf("width %d dropped the posture: %q", w, out)
		}
		for _, seg := range []string{"claude-opus-5-5", "@anthropic", "TASK-7", "2 granted", "3 sessions", "step 4", "1.2k↓ 300↑"} {
			if strings.Contains(last, seg) && !strings.Contains(out, seg) {
				// Gone at this width: it must be gone whole, not cut.
				for cut := len(seg) - 1; cut > 3; cut-- {
					if strings.Contains(out, seg[:cut]+"…") {
						t.Fatalf("width %d cut %q instead of dropping it: %q", w, seg, out)
					}
				}
			}
		}
		if strings.Contains(out, "claude-opus-5-5") && !strings.Contains(out, "2 granted") {
			t.Fatalf("width %d kept the model and dropped the grant count: %q", w, out)
		}
		last = out
	}
	// With room, everything is there, and the bar no longer claims a clean pass
	// for a session that is merely idle.
	b := render.NewBuffer(140, 1)
	DrawStatusBar(b, b.Bounds(), th, st, 0, nil, false)
	if out := bufferString(b); strings.Contains(out, th.Glyphs().Pass) || !strings.Contains(out, "claude-opus-5-5@anthropic") {
		t.Fatalf("wide bar: %q", out)
	}
}

// What each construct of a reply draws.
func TestMarkdownConstructs(t *testing.T) {
	th := unicodeDark()
	text := func(md string, width int) []string {
		var out []string
		for _, l := range renderMarkdown(md, th, width, false) {
			out = append(out, strings.TrimRight(l.Text(), " "))
		}
		return out
	}
	eq := func(name string, got, want []string) {
		t.Helper()
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, got, want)
		}
	}
	eq("headings lose their marks", text("# One\n## Two\n### Three", 40), []string{"One", "Two", "Three"})
	eq("a heading wraps", text("## a heading that is longer than the pane", 20), []string{"a heading that is", "longer than the pane"})
	eq("a list item wraps beside its marker", text("- alpha beta gamma delta", 14), []string{"• alpha beta", "  gamma delta"})
	eq("a nested item keeps its depth", text("- top\n  - under", 30), []string{"• top", "  • under"})
	eq("a numbered item keeps its number", text("12. twelfth item here", 14), []string{"12. twelfth", "    item here"})
	eq("a quote takes a rule", text("> quoted words", 30), []string{"│ quoted words"})
	eq("a rule", text("---", 10), []string{strings.Repeat("─", 10)})
	eq("a fence draws no rows of its own", text("```go\nx := 1\n```", 30), []string{"│ x := 1"})
	eq("a long code line wraps", text("```\naaaa bbbb cccc dddd\n```", 12), []string{"│ aaaa bbbb", "│ cccc dddd"})
	eq("a plus is prose", text("+ not a diff", 30), []string{"+ not a diff"})

	// A diff is one where it says it is.
	rows := renderMarkdown("```diff\n-old\n+new\n ctx\n```", th, 30, false)
	if rows[0][1].Style.Fg != th.Danger || rows[1][1].Style.Fg != th.Success || rows[2][1].Style.Fg == th.Success {
		t.Errorf("a diff fence is not coloured as a diff: %+v", rows)
	}
	// Code is set apart by the rule in front of it, on the transcript's own
	// ground: nothing in a reply sits on a fill, a code block and a word of
	// inline code included.
	for _, src := range []string{"```\nx\n```", "run `go vet` first", "```diff\n+new\n```"} {
		for _, row := range renderMarkdown(src, th, 20, false) {
			for _, span := range row {
				if span.Style.Bg != th.Bg {
					t.Errorf("%q draws %q on %+v, not the transcript's ground", src, span.Text, span.Style.Bg)
				}
			}
		}
	}
	if code := renderMarkdown("```\nx\n```", th, 20, false)[0]; !strings.HasPrefix(code.Text(), th.Glyphs().VBar+" x") {
		t.Errorf("a code row is not behind its rule: %q", code.Text())
	}
}

// What a tool printed is drawn as it printed it; a diff in it is coloured from
// its first hunk on, with every line's sign left in its column.
func TestToolOutputIsNotMarkdownAndADiffIsColouredOnceItIsOne(t *testing.T) {
	th := unicodeDark()
	plain := th.Muted()
	out := "- a yaml item\n--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n ctx\n-old\n+new\n**not bold**"
	rows := renderPre(out, th, 60, plain)
	if got := rows[0][0]; got.Text != "- a yaml item" || got.Style != plain {
		t.Fatalf("a line before any hunk is styled: %+v", got)
	}
	fg := func(i int) render.Color { return rows[i][0].Style.Fg }
	// The hunk header, a removed line, an added line; the context line between
	// them stays in the output's own style.
	if fg(3) != th.Info || fg(5) != th.Danger || fg(6) != th.Success || rows[4][0].Style != plain {
		t.Fatalf("the diff is not coloured from its hunk on")
	}
	if rows[5][0].Text != "-old" || rows[6][0].Text != "+new" {
		t.Fatalf("a diff line was rewritten: %q %q", rows[5][0].Text, rows[6][0].Text)
	}
	if rows[7][0].Text != "**not bold**" {
		t.Fatalf("inline markup was applied to tool output: %q", rows[7][0].Text)
	}
}

// Every glyph this face added has a fallback a terminal without Unicode can draw.
func TestNewGlyphsHaveASCIIFallbacks(t *testing.T) {
	g := NoColorTheme().Glyphs()
	for name, s := range map[string]string{"prompt": g.Prompt, "thought": g.Thought, "dot": g.Dot} {
		if s == "" {
			t.Errorf("no %s glyph", name)
		}
		for _, r := range s {
			if r > 127 {
				t.Errorf("the %s fallback %q is not ASCII", name, s)
			}
		}
	}
	// And the rows they sit on draw with it: no rule, bullet or mark outside ASCII.
	th := NoColorTheme()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "ask"})
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "think"})
	s.Append(ui.Event{Kind: ui.KindText, Text: "- item\n> quote\n---\n```\ncode\n```\n"})
	s.Append(ui.Event{Kind: ui.KindUsage, InputTokens: 1, OutputTokens: 2, Text: "$0.01 m"})
	for _, row := range frameOf(s, th, 60, 16) {
		for _, r := range row {
			if r > 127 {
				t.Fatalf("%q in an ASCII frame: %q", r, row)
			}
		}
	}
}

func TestElapsed(t *testing.T) {
	for secs, want := range map[int]string{0: "0s", 59: "59s", 60: "1m 00s", 125: "2m 05s", 3600: "1h 00m", 3725: "1h 02m"} {
		if got := elapsed(secs); got != want {
			t.Errorf("elapsed(%d) = %q, want %q", secs, got, want)
		}
	}
}

// A finished thought is its heading. The first words of the thinking are not
// printed beside it: that was a line of reply nobody asked to read.
func TestAFinishedThoughtIsItsHeadingAlone(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "The user is asking about the paste reader."})
	s.Append(ui.Event{Kind: ui.KindReasoning, Agent: "worker-1", Text: "a child's plan"})
	s.Append(ui.Event{Kind: ui.KindTurnEnd})
	s.Append(ui.Event{Kind: ui.KindText, Text: "done"})
	own := rowsOf(s.Entries()[0], th, 80)
	if len(own) != 1 || strings.TrimSpace(own[0]) != th.Glyphs().Bullet+"  Thought" {
		t.Fatalf("a finished thought drew %q", own)
	}
	// Whose thought it was is still said.
	if child := rowsOf(s.Entries()[1], th, 80); len(child) != 1 || !strings.Contains(child[0], "[worker-1] Thought") {
		t.Fatalf("a child's finished thought drew %q", child)
	}
}

// A result whose call is still drawn above it, with a decision between them,
// does not name the call a second time.
func TestAResultUnderItsCallDoesNotRepeatIt(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "exec", Arguments: []byte(`{"command":"go test ./..."}`)})
	s.Append(ui.Event{Kind: ui.KindPolicy, Rule: "exec.allowlist", Severity: "warn", Text: "allowed by the allowlist"})
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "exec", IsError: true, Text: "--- FAIL: TestX\nFAIL\n"})
	rows := s.rows(th, 80)
	if len(rows) != 4 {
		t.Fatalf("drew %d rows, want the call, the decision and two of output", len(rows))
	}
	if got := rows[2].line.Text(); !strings.HasPrefix(got, th.Glyphs().Block) || !strings.Contains(got, "--- FAIL: TestX") {
		t.Fatalf("the failure does not open with what failed: %q", got)
	}
	for _, r := range rows[2:] {
		if strings.Contains(r.line.Text(), "exec") {
			t.Fatalf("the result names its call again: %q", r.line.Text())
		}
	}
	// On a row too narrow for both, the rule moves under the decision whole.
	narrow := rowsOf(s.Entries()[1], th, 34)
	if len(narrow) != 2 || strings.TrimSpace(narrow[1]) != "exec.allowlist (warn)" {
		t.Fatalf("a narrow decision drew %q", narrow)
	}
}

// The turn's closing line: counts a reader can read, and the cost beside them.
func TestTheUsageLineGroupsItsCounts(t *testing.T) {
	th := unicodeDark()
	one := &Entry{Event: ui.Event{Kind: ui.KindUsage, InputTokens: 1218234, OutputTokens: 912, Text: "$0.31 claude-opus-5-5"}}
	if got := rowsOf(one, th, 80); len(got) != 1 || strings.TrimSpace(got[0]) != "1,218,234 in / 912 out · $0.31 claude-opus-5-5" {
		t.Fatalf("usage drew %q", got)
	}
	// Split by model, each model keeps a row.
	split := &Entry{Event: ui.Event{Kind: ui.KindUsage, InputTokens: 5500, OutputTokens: 550, Text: "big · $0.08\nsmall · $0.01"}}
	if got := rowsOf(split, th, 80); len(got) != 3 || !strings.Contains(got[0], "5,500 in") {
		t.Fatalf("split usage drew %q", got)
	}
}

// The session's opening is one row where the pane is wide enough for the mark
// and the quick actions together, and two where it is not.
func TestTheSessionOpeningIsOneRowWhereItFits(t *testing.T) {
	th := unicodeDark()
	e := &Entry{Event: ui.Event{Kind: ui.KindSessionStart, Posture: "strict", Model: "some-model"}}
	wide := rowsOf(e, th, 120)
	if len(wide) != 1 || !strings.Contains(wide[0], "some-model") || !strings.HasSuffix(wide[0], "Ctrl+P palette") {
		t.Fatalf("a wide opening drew %q", wide)
	}
	mid := rowsOf(&Entry{Event: e.Event}, th, 70)
	if len(mid) != 2 || !strings.Contains(mid[1], "/pull /push /issues") || !strings.Contains(mid[1], "Ctrl+P palette") {
		t.Fatalf("a 70-column opening drew %q", mid)
	}
	// Narrower than the guide: the commands stay, whole, and nothing is cut
	// with an ellipsis.
	narrow := rowsOf(&Entry{Event: e.Event}, th, 50)
	if len(narrow) != 2 || !strings.HasSuffix(narrow[1], "/doctor /tools") || strings.Contains(narrow[1], "…") {
		t.Fatalf("a 50-column opening drew %q", narrow)
	}
}

// The key row names the keys of the moment and always the way to the rest. It
// does not carry ctrl+c, which the working row offers while there is a turn to
// stop, the session keys, which the palette and the help list, or enter, which
// the composer it sits under is for.
func TestTheKeyRowIsShortAndAlwaysPointsToTheRest(t *testing.T) {
	th := Dark()
	b := render.NewBuffer(120, 1)
	DrawShortcutBar(b, b.Bounds(), th, CtxPrompt)
	got := strings.Join(strings.Fields(bufferString(b)), " ")
	if got != "tab focus ctrl+p commands F1 more" {
		t.Fatalf("the key row reads %q", got)
	}
	// Every key it left out is still bound and still listed by the help.
	for _, label := range []string{"send", "cancel turn", "quit", "dashboard", "sessions"} {
		found := false
		for _, bd := range allBindings() {
			found = found || bd.Label == label
		}
		if !found {
			t.Errorf("%q is no longer bound", label)
		}
	}
}

// hueFamily names the family a colour reads as: a grey, a red, a blue, a green,
// or something else. A colour with little difference between its channels is a
// grey whatever its hue.
func hueFamily(c render.Color) string {
	r8, g8, b8, ok := c.RGBA()
	if !ok {
		return "neutral"
	}
	r, g, b := float64(r8), float64(g8), float64(b8)
	hi, lo := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	if hi-lo < 48 {
		return "neutral"
	}
	var h float64
	switch hi {
	case r:
		h = math.Mod((g-b)/(hi-lo), 6)
	case g:
		h = (b-r)/(hi-lo) + 2
	default:
		h = (r-g)/(hi-lo) + 4
	}
	if h *= 60; h < 0 {
		h += 360
	}
	switch {
	case h >= 330 || h <= 25:
		return "red"
	case h >= 195 && h <= 240:
		return "blue"
	case h >= 90 && h <= 160:
		return "green"
	}
	return "other"
}

// paletteSession is one session holding every outcome the transcript and the
// chrome can draw, under a posture that is not strict.
func paletteSession(th Theme) *App {
	a := NewApp(th, &stubHost{})
	a.Dispatch(ActionResize{W: 120, H: 60})
	a.AddSession("S1", "first")
	v := a.Current()
	for _, ev := range []ui.Event{
		{Kind: ui.KindSessionStart, Posture: "dev", Model: "some-model"},
		{Kind: ui.KindTurnStart, Text: "fix the reader"},
		{Kind: ui.KindReasoning, Text: "hm"},
		{Kind: ui.KindToolStart, Tool: "read_file", Arguments: []byte(`{"path":"a.go"}`)},
		{Kind: ui.KindToolResult, Text: "package a\n"},
		{Kind: ui.KindToolStart, Tool: "devcouncil_exec_command", Arguments: []byte(`{"command":"go test"}`)},
		{Kind: ui.KindPolicy, Rule: "exec.allowlist", Severity: "soft", Text: "allowed by the allowlist"},
		{Kind: ui.KindToolResult, IsError: true, Text: "--- FAIL: TestX\nFAIL\n"},
		{Kind: ui.KindPolicy, Rule: "path.secret", Severity: "hard", Text: "refused: a credential path"},
		{Kind: ui.KindToolStart, Tool: "devcouncil_patch_file", Arguments: []byte(`{"path":"a.go","target_content":"x\n","replacement_content":"y\nz\n"}`)},
		{Kind: ui.KindToolResult, Text: "patched\n"},
		{Kind: ui.KindToolStart, Tool: "devcouncil_spawn_subagents", Arguments: []byte(`{"tasks":[{"label":"reader"}]}`)},
		{Kind: ui.KindToolResult, Text: "done\n"},
		{Kind: ui.KindToolStart, Tool: "edit_file", Arguments: []byte(`{"path":"a.go"}`)},
		{Kind: ui.KindToolResult, Text: "edited\n", Rule: "path.protected", GrantID: "G-1", GrantedBy: "human"},
		{Kind: ui.KindToolResult, Text: "verified\n", Degraded: []string{"secret_scan"}},
		{Kind: ui.KindToolResult, Text: "wrote\n", Rule: "scope", Demoted: "dev", Weakened: []string{"sandbox"}},
		{Kind: ui.KindToolResult, Text: "diff --git a/a.go b/a.go\n@@ -1 +1 @@\n-old\n+new\n"},
		{Kind: ui.KindText, Text: "Done: `flush()` is **called**.\n\n- one\n1. two\n> quote\n\n```go\nif s == \"x\" { // why\n\treturn nil\n}\n```\n\n```diff\n-a\n+b\n```\n"},
		{Kind: ui.KindLease, Text: "lease renewed"},
		{Kind: ui.KindNotice, Text: "a notice"},
		{Kind: ui.KindError, Text: "the turn failed"},
		{Kind: ui.KindUsage, InputTokens: 10, OutputTokens: 2, Text: "$0.01 m"},
	} {
		v.Apply(ev)
	}
	v.Scroll.SetFoldAll(false)
	v.Status.Model, v.Status.Provider = "some-model", "anthropic"
	v.Status.TaskID, v.Status.LeaseExpiry = "TASK-1", time.Now().Add(time.Minute)
	v.Status.GitBranch, v.Status.GitDirty, v.Status.Grants, v.Status.Steps = "main", 2, 1, 3
	v.Status.ContextUsed, v.Status.ContextMax = 75, 100
	v.Status.InputTokens, v.Status.OutputTokens = 10, 2
	return a
}

// The frame is the brand's red, one blue, greys, and the failure red. Green is
// a line a diff adds and nothing else. No colour is ever a fill: the posture,
// the row that says a run is not strict, a blocked session and a failed one are
// words on the surface they sit on.
func TestTheFrameKeepsToItsPalette(t *testing.T) {
	for _, th := range []Theme{Dark(), Light()} {
		th.Unicode = true
		a := paletteSession(th)
		check := func(view string) {
			t.Helper()
			b := render.NewBuffer(120, 60)
			a.Draw(b)
			rows := strings.Split(bufferString(b), "\n")
			for y := 0; y < 60; y++ {
				added := strings.HasPrefix(strings.TrimLeft(rows[y], " │|"), "+")
				// How many lines an edit added is the other place a green is
				// allowed: the word a green cell is in starts with the plus.
				word := func(x int) string {
					for x > 0 && b.Cell(x-1, y).R != ' ' {
						x--
					}
					return string(b.Cell(x, y).R)
				}
				for x := 0; x < 120; x++ {
					st := b.Cell(x, y).Style
					switch fam := hueFamily(st.Fg); {
					case b.Cell(x, y).R == ' ':
						// A blank has no foreground to see.
					case fam == "neutral", fam == "red", fam == "blue":
					case fam == "green" && (added || word(x) == "+"):
					default:
						t.Fatalf("%s theme, %s: %q at %d,%d is drawn in a %s:\n%s", th.Name, view, string(b.Cell(x, y).R), x, y, fam, rows[y])
					}
					for name, fill := range map[string]render.Color{"failure": th.Danger, "accent": th.Accent, "blue": th.Info, "green": th.Success} {
						if st.Bg == fill {
							t.Fatalf("%s theme, %s: the %s colour fills the cell at %d,%d:\n%s", th.Name, view, name, x, y, rows[y])
						}
					}
					if fam := hueFamily(st.Bg); fam != "neutral" && fam != "red" {
						t.Fatalf("%s theme, %s: a %s background at %d,%d:\n%s", th.Name, view, fam, x, y, rows[y])
					}
				}
			}
		}

		rows := screen(a, 120, 60)
		for _, want := range []string{"dev", "not a strict run", "cleared by G-1", "checks that did not run: secret_scan", "+new", "+b", "Edited a.go  +2  -1", "Ran 1 subagent reader", "Ran go test"} {
			if !contains2(rows, want) {
				t.Fatalf("%s theme: the frame does not show %q:\n%s", th.Name, want, strings.Join(rows, "\n"))
			}
		}
		check("session")

		// A turn that has gone quiet, a search with matches, and a second
		// session blocked on the operator.
		a.Current().lastActivity = time.Now().Add(-time.Minute)
		a.Current().SetBusy(true, "working")
		a.Current().Scroll.SetSearch("the", th)
		check("stalled, searching")
		a.AddSession("S2", "second")
		a.Dispatch(ActionApprovalRequest{SessionID: "S2", Request: blockedRequest(true), Reply: make(chan ui.Decision, 1)})
		check("approval card")
		key(a, "ctrl+g")
		if rows := screen(a, 120, 60); !contains2(rows, "blocked") {
			t.Fatalf("the dashboard does not show the blocked session:\n%s", strings.Join(rows, "\n"))
		}
		check("dashboard")
	}
}

// The posture is read off the bar by weight: strict is quiet, a relaxed posture
// is the text colour, and only the one that has stopped asking is the failure
// colour.
func TestThePostureIsAWordNotAChip(t *testing.T) {
	th := Dark()
	style := func(posture string) render.Style {
		b := render.NewBuffer(80, 1)
		DrawStatusBar(b, b.Bounds(), th, Status{Posture: posture, Model: "m"}, 0, nil, false)
		x := strings.Index(bufferString(b), posture)
		if x < 0 {
			t.Fatalf("the bar does not show %q", posture)
		}
		return b.Cell(x, 0).Style
	}
	strict, dev, yolo := style("strict"), style("dev"), style("yolo")
	if strict.Fg != th.FgMuted || dev.Fg != th.Fg || yolo.Fg != th.Danger {
		t.Fatalf("posture colours: strict %v, dev %v, yolo %v", strict.Fg, dev.Fg, yolo.Fg)
	}
	for name, s := range map[string]render.Style{"strict": strict, "dev": dev, "yolo": yolo} {
		if s.Bg != th.Bg {
			t.Fatalf("the %s posture sits on a fill", name)
		}
	}
}

// A call reads as what was done and to what, not as a tool's name and a list of
// its arguments: reading a file, editing one and how much, running a command.
func TestACallReadsAsWhatWasDone(t *testing.T) {
	th := unicodeDark()
	row := func(tool, args, result string, failed bool) (running, done string, line render.Line) {
		s := NewScrollback()
		s.Append(ui.Event{Kind: ui.KindToolStart, Tool: tool, Arguments: []byte(args)})
		running = strings.TrimSpace(s.rows(th, 100)[0].line.Text())
		s.Append(ui.Event{Kind: ui.KindToolResult, Text: result, IsError: failed})
		rows := s.rows(th, 100)
		return running, strings.TrimSpace(rows[0].line.Text()), rows[0].line
	}
	// A call that is running keeps the tool mark; a result that passed has the
	// routine one.
	pass, tool := th.Glyphs().Bullet, th.Glyphs().Tool
	for _, c := range []struct {
		tool, args, result string
		running, done      string
	}{
		{"devcouncil_read_file", `{"path":"ui/reader.go","limit":120}`, "a\nb\nc\n",
			tool + "  Reading ui/reader.go", pass + "  Read ui/reader.go  · 3 lines"},
		{"devcouncil_patch_file", `{"path":"ui/reader.go","target_content":"a\nold\nz\n","replacement_content":"a\nnew\nmore\nz\n"}`, "patched ui/reader.go",
			tool + "  Editing ui/reader.go", pass + "  Edited ui/reader.go  +2  -1"},
		{"devcouncil_write_file", `{"path":"ui/new.go","content":"package ui\n\nvar x = 1\n"}`, "wrote ui/new.go (24 bytes)",
			tool + "  Writing ui/new.go", pass + "  Wrote ui/new.go  +3"},
		{"devcouncil_delete_file", `{"path":"ui/old.go"}`, "deleted ui/old.go",
			tool + "  Deleting ui/old.go", pass + "  Deleted ui/old.go"},
		// A file tool that says anything but that it did it is heard.
		{"devcouncil_write_file", `{"path":"ui/new.go","content":"x\n"}`, "kept: the file already holds this",
			tool + "  Writing ui/new.go", pass + "  Wrote ui/new.go  +1  · kept: the file already holds this"},
		{"devcouncil_patch_file", `{"path":"ui/reader.go","target_content":"a\n","replacement_content":"b\n"}`, "patched ui/reader.go (9 bytes)\nwarning: mixed line endings",
			tool + "  Editing ui/reader.go", pass + "  Edited ui/reader.go  +1  -1  · 2 lines"},
		// A command that prints the same words is not a file tool confirming itself.
		{"devcouncil_exec_command", `{"command":"./fix.sh"}`, "patched ui/reader.go",
			tool + "  Running ./fix.sh", pass + "  Ran ./fix.sh  · patched ui/reader.go"},
		{"devcouncil_exec_command", `{"command":"go test ./ui/..."}`, "ok  \tui\t0.4s\nok  \tui/tui\t9s\n",
			tool + "  Running go test ./ui/...", pass + "  Ran go test ./ui/...  · 2 lines"},
		{"devcouncil_grep", `{"pattern":"flush\\(","path":"ui/input"}`, "a.go:1\nb.go:2\n",
			tool + "  Searching for flush\\( in ui/input", pass + "  Searched for flush\\( in ui/input  · 2 lines"},
		{"devcouncil_git_status", `{}`, " M a.go\n",
			tool + "  Running git status", pass + "  Ran git status  · M a.go"},
		{"devcouncil_git_commit", `{"message":"fix the tail"}`, "abc123\n",
			tool + "  Running git commit  message=fix the tail", pass + "  Ran git commit  message=fix the tail  · abc123"},
		// A tool with no entry of its own keeps its name and its arguments.
		{"jira_search", `{"query":"paste","limit":5}`, "one\ntwo\n",
			tool + "  Using jira_search  paste  limit=5", pass + "  Used jira_search  paste  limit=5  · 2 lines"},
		// Other agents, skills, the task board, and a script.
		{"devcouncil_spawn_subagents", `{"tasks":[{"label":"reader","prompt":"x"},{"label":"tests","prompt":"y"}]}`, "reader: done\ntests: done\n",
			tool + "  Starting 2 subagents reader, tests", pass + "  Ran 2 subagents reader, tests  · 2 lines"},
		{"devcouncil_load_skill", `{"name":"devmap-impact"}`, "loaded\n",
			tool + "  Loading skill devmap-impact", pass + "  Loaded skill devmap-impact  · loaded"},
		{"devcouncil_activate_tools", `{"tools":["devcouncil_grep"]}`, "ok\n",
			tool + "  Enabling 1 tool devcouncil_grep", pass + "  Enabled 1 tool devcouncil_grep  · ok"},
		{"devcouncil_checkout_task", `{"task_id":"TASK-7"}`, "leased\n",
			tool + "  Checking out TASK-7", pass + "  Checked out TASK-7  · leased"},
		{"devcouncil_verify_task", `{}`, "3 checks passed\n",
			tool + "  Verifying the task", pass + "  Verified the task  · 3 checks passed"},
		{"devcouncil_exec_command", `{"command":"./scripts/build.sh --release"}`, "built\n",
			tool + "  Running ./scripts/build.sh --release", pass + "  Ran ./scripts/build.sh --release  · built"},
	} {
		running, done, _ := row(c.tool, c.args, c.result, false)
		if running != c.running || done != c.done {
			t.Errorf("%s:\n running %q\n    want %q\n    done %q\n    want %q", c.tool, running, c.running, done, c.done)
		}
	}

	// What a call worked on is in the one colour for that, and the size of an
	// edit in the colours of a diff.
	_, _, line := row("devcouncil_patch_file", `{"path":"a.go","target_content":"x\n","replacement_content":"y\nz\n"}`, "ok", false)
	colours := map[string]render.Color{}
	for _, span := range line {
		colours[strings.TrimSpace(span.Text)] = span.Style.Fg
	}
	if colours["Edited"] != th.FgMuted || colours["a.go"] != th.Info || colours["+2"] != th.Success || colours["-1"] != th.Danger {
		t.Fatalf("an edit's row is coloured %v", colours)
	}
	// A file the turn changed is the one thing in the work log that is
	// coloured. What it read, ran or searched is a step quieter than the reply.
	for _, c := range []struct{ tool, args, word string }{
		{"devcouncil_read_file", `{"path":"a.go"}`, "a.go"},
		{"devcouncil_exec_command", `{"command":"gofmt"}`, "gofmt"},
		{"devcouncil_grep", `{"pattern":"flush","path":"ui"}`, "flush"},
	} {
		_, _, line := row(c.tool, c.args, "one\ntwo\n", false)
		for _, span := range line {
			if strings.TrimSpace(span.Text) == c.word && span.Style.Fg != th.FgMuted {
				t.Fatalf("%s draws %q in %v, want the quiet text colour", c.tool, c.word, span.Style.Fg)
			}
			if strings.TrimSpace(span.Text) != "" && (span.Style.Fg == th.Fg || span.Style.Attrs&render.Bold != 0) {
				t.Fatalf("%s draws %q as loud as the reply", c.tool, span.Text)
			}
		}
	}
	// Sixteen colours have one dark grey, which is a label's colour and not a
	// row's: there the log keeps the text colour.
	low := PickThemeByNameUnchecked("dark", render.ANSI16, true)
	ls := NewScrollback()
	ls.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_read_file", Arguments: []byte(`{"path":"a.go"}`)})
	for _, span := range ls.rows(low, 80)[0].line {
		if strings.Contains(span.Text, "Reading") && span.Style.Fg != low.Fg {
			t.Fatalf("at sixteen colours a call is drawn in %v", span.Style.Fg)
		}
	}

	// A patch allowed to replace its target wherever it occurs has no size: how
	// many places that was is not in the call.
	_, many, _ := row("devcouncil_patch_file", `{"path":"a.go","target_content":"x\n","replacement_content":"y\n","allow_multiple":true}`, "patched a.go", false)
	if strings.Contains(many, "+1") || strings.Contains(many, "-1") {
		t.Fatalf("a multiple replace was sized: %q", many)
	}

	// An edit that failed does not say it edited, or how much, and keeps what
	// the tool said: one line of it on the call's row.
	_, failed, _ := row("devcouncil_patch_file", `{"path":"a.go","target_content":"x\n","replacement_content":"y\n"}`, "target not found", true)
	if failed != th.Glyphs().Block+"  Editing a.go  · target not found" {
		t.Fatalf("a failed edit reads %q", failed)
	}

	// The confirmation left off the row is still there when the row is opened.
	opened := NewScrollback()
	opened.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_patch_file", Arguments: []byte(`{"path":"a.go","target_content":"x\n","replacement_content":"y\n"}`)})
	opened.Append(ui.Event{Kind: ui.KindToolResult, Text: "patched a.go (2 bytes)"})
	opened.entries[1].Folded = false
	opened.entries[1].Invalidate()
	var got []string
	for _, r := range opened.rows(th, 100) {
		got = append(got, r.line.Text())
	}
	if all := strings.Join(got, "\n"); !strings.Contains(all, "patched a.go (2 bytes)") {
		t.Fatalf("an opened edit lost what the tool said:\n%s", all)
	}

	// The lines an edit quotes around its change are not counted as changed.
	if added, removed, ok := editSize(map[string]any{"target_content": "a\nb\nc\n", "replacement_content": "a\nB\nc\n"}); !ok || added != 1 || removed != 1 {
		t.Fatalf("a one-line change inside three lines is +%d -%d", added, removed)
	}
	if _, _, ok := editSize(map[string]any{"path": "a.go"}); ok {
		t.Fatal("a call that is not an edit was sized as one")
	}
}

// The working row words a running call the way the transcript does.
func TestTheWorkingRowNamesWhatIsBeingDone(t *testing.T) {
	a, _ := newTestApp()
	v := a.Current()
	v.SetBusy(true, "working")
	v.Apply(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_read_file", Arguments: []byte(`{"path":"ui/reader.go"}`)})
	if rows := screen(a, 100, 30); !contains2(rows, "Reading ui/reader.go") {
		t.Fatalf("the working row does not say what is being read:\n%s", strings.Join(rows, "\n"))
	}
}

// The working row scrolls a label only when the row has no room for it: one
// that fits beside the note is drawn whole, however the motion is set.
func TestTheWorkingRowDoesNotCutALabelThatFits(t *testing.T) {
	th := unicodeDark()
	label := "Starting 2 subagents trace-decoder, audit-callers"
	st := Status{Busy: true, BusyLabel: label}
	for _, tick := range []int{0, 7, 40} {
		b := render.NewBuffer(96, 1)
		DrawActivity(b, render.Rect{X: 0, Y: 0, W: 96, H: 1}, th, st, tick, true)
		if out := bufferString(b); !strings.Contains(out, label+"  (ctrl+c to interrupt)") {
			t.Fatalf("tick %d cut a label the row had room for: %q", tick, out)
		}
	}
	// One the row cannot hold still leaves the way to stop the turn on screen.
	b := render.NewBuffer(40, 1)
	DrawActivity(b, render.Rect{X: 0, Y: 0, W: 40, H: 1}, th, st, 0, true)
	if out := bufferString(b); !strings.Contains(out, "(ctrl+c to interrupt)") {
		t.Fatalf("a narrow working row lost how to stop the turn: %q", out)
	}
}

// A call that keeps its own row, because a decision sits between it and its
// result, does not go on saying it is running once the result is in.
func TestACallWithItsOwnRowIsPastTenseOnceAnswered(t *testing.T) {
	th := unicodeDark()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_exec_command", Arguments: []byte(`{"command":"go test ./..."}`)})
	s.Append(ui.Event{Kind: ui.KindPolicy, Rule: "exec.allowlist", Severity: "soft", Text: "allowed by the allowlist"})
	if got := s.rows(th, 80)[0].line.Text(); !strings.Contains(got, "Running go test ./...") {
		t.Fatalf("a call waiting on its result reads %q", got)
	}
	s.Append(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "FAIL\n"})
	rows := s.rows(th, 80)
	if got := rows[0].line.Text(); !strings.Contains(got, "Ran go test ./...") || strings.Contains(got, "Running") {
		t.Fatalf("an answered call still reads %q", got)
	}
	if len(rows) != 3 {
		t.Fatalf("drew %d rows, want the call, the decision and the failure", len(rows))
	}
}

// Nothing is pinned to the top of the screen. What the session is sits on one
// row under the composer, on the screen's own ground, beside the keys of the
// moment; the top row is the transcript's.
func TestTheFrameHasNoTopBarAndOneQuietFooter(t *testing.T) {
	a, _ := newTestApp()
	v := a.Current()
	v.Status.Model, v.Status.Posture = "claude-opus-5-5", "strict"
	v.Status.GitBranch = "main"
	v.Apply(ui.Event{Kind: ui.KindTurnStart, Text: "hello"})
	const w, h = 100, 24
	b := render.NewBuffer(w, h)
	a.Dispatch(ActionResize{W: w, H: h})
	a.Draw(b)
	row := func(y int) string {
		var sb strings.Builder
		for x := 0; x < w; x++ {
			if r := b.Cell(x, y).R; r != 0 {
				sb.WriteRune(r)
			}
		}
		return sb.String()
	}
	if top := row(0); strings.Contains(top, "strict") || strings.Contains(top, "claude-opus-5-5") {
		t.Fatalf("the status is still on the top row: %q", top)
	}
	foot := row(h - 1)
	for _, want := range []string{"strict", "claude-opus-5-5", "main", "ctrl+p commands", "F1 more"} {
		if !strings.Contains(foot, want) {
			t.Fatalf("the footer is missing %q: %q", want, foot)
		}
	}
	th := a.Theme
	for x := 0; x < w; x++ {
		if c := b.Cell(x, h-1); c.Style.Bg != th.Bg {
			t.Fatalf("the footer is on a fill at column %d: %+v", x, c.Style.Bg)
		}
		if c := b.Cell(x, h-1); c.R > ' ' && c.Style.Attrs&render.Bold != 0 {
			t.Fatalf("a strict session with nothing to flag draws %q in bold on the footer", string(c.R))
		}
	}
	// The composer's frame is a neutral rule, not the brand colour: focus is
	// the prompt mark inside it.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := b.Cell(x, y)
			if strings.ContainsRune("╭╮╰╯─│", c.R) && c.Style.Fg == th.BorderFocus {
				t.Fatalf("the composer frame is drawn in the brand colour at %d,%d", x, y)
			}
		}
	}
}

// What needs seeing on the footer is still drawn strong, and a narrow footer
// keeps the keys and the notices before it keeps the status.
func TestTheFooterKeepsWhatMatters(t *testing.T) {
	th := Dark()
	draw := func(w int, st Status, extra ...string) (string, *render.Buffer) {
		b := render.NewBuffer(w, 1)
		DrawFooter(b, b.Bounds(), th, st, CtxPrompt, 0, nil, false, extra...)
		return strings.Join(strings.Fields(bufferString(b)), " "), b
	}
	st := Status{Posture: "strict", Model: "claude-opus-5-5", GitBranch: "main", Grants: 2}
	wide, b := draw(110, st)
	if !strings.Contains(wide, "2 granted") || !strings.Contains(wide, "F1 more") {
		t.Fatalf("a wide footer reads %q", wide)
	}
	x := strings.Index(bufferString(b), "granted")
	if b.Cell(x, 0).Style.Attrs&render.Bold == 0 || b.Cell(x, 0).Style.Fg != th.Fg {
		t.Fatalf("a grant on the footer is drawn %+v, want it strong", b.Cell(x, 0).Style)
	}
	// Narrow: the notice and the way to the rest of the keys survive, and the
	// posture is the last of the status to go.
	narrow, _ := draw(64, st, "2 more approval(s) waiting")
	if !strings.Contains(narrow, "2 more approval(s) waiting") || !strings.Contains(narrow, "strict") {
		t.Fatalf("a narrow footer reads %q", narrow)
	}
	tiny, _ := draw(40, st, "scrolled back — G for latest")
	if !strings.Contains(tiny, "scrolled back") {
		t.Fatalf("a footer too narrow for both dropped the notice: %q", tiny)
	}
}

// A run that is not strict says so on a row of its own above the composer, and
// a passing message at the top of the screen cannot cover it.
func TestTheNotStrictRowSitsAboveTheComposer(t *testing.T) {
	a, _ := newTestApp()
	v := a.Current()
	v.Status.Posture, v.Status.Grants = "strict", 1
	const w, h = 100, 24
	a.Dispatch(ActionResize{W: w, H: h})
	a.setNotice("copied", StatusInfo)
	rows := screen(a, w, h)
	at := -1
	for y, r := range rows {
		if strings.Contains(r, "not a strict run") {
			at = y
		}
	}
	if at < 0 {
		t.Fatalf("the frame does not say the run is not strict:\n%s", strings.Join(rows, "\n"))
	}
	if at < h/2 || at >= h-1 {
		t.Fatalf("the not-strict row is on row %d of %d, want it above the composer", at, h)
	}
	if !strings.Contains(rows[0], "copied") {
		t.Fatalf("the passing message is not on the top row: %q", rows[0])
	}
}

// A routine row has one quiet mark, and a row with something to flag — a call
// with no result among them — keeps a mark of its own shape. Without Unicode
// the marks are words and stay.
func TestRoutineRowsShareOneQuietMark(t *testing.T) {
	th := unicodeDark()
	g := th.Glyphs()
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "thinking"})
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_read_file", Arguments: []byte(`{"path":"a.go"}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, Text: "x\ny\n"})
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_exec_command", Arguments: []byte(`{"command":"go test"}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "FAIL\nFAIL\n"})
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_write_file", Arguments: []byte(`{"path":"b.go","content":"x\n"}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, Text: "wrote b.go (2 bytes)", GrantID: "G-1", GrantedBy: "human", Rule: "scope"})
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "devcouncil_list_dir", Arguments: []byte(`{"path":"."}`)})
	var marks []string
	for _, r := range s.rows(th, 80) {
		text := r.line.Text()
		if strings.HasPrefix(text, " ") || text == "" {
			continue
		}
		mark := strings.Fields(text)[0]
		marks = append(marks, mark)
		if mark == g.Bullet && r.line[0].Style.Fg != th.FgSubtle {
			t.Fatalf("the routine mark is drawn in %v", r.line[0].Style.Fg)
		}
	}
	// The last call has no result: it is running, or never came back, and
	// keeps the tool mark so it does not read as one of the passes above it.
	want := []string{g.Bullet, g.Bullet, g.Block, g.Granted, g.Tool}
	if strings.Join(marks, " ") != strings.Join(want, " ") {
		t.Fatalf("the marks down the log are %q, want %q", marks, want)
	}

	plain := NoColorTheme()
	pg := plain.Glyphs()
	var words []string
	for _, r := range s.rows(plain, 80) {
		if text := r.line.Text(); text != "" && !strings.HasPrefix(text, " ") {
			words = append(words, strings.Fields(text)[0])
		}
	}
	if got, want := strings.Join(words, " "), strings.Join([]string{pg.Thought, pg.Pass, pg.Block, pg.Granted, pg.Tool}, " "); got != want {
		t.Fatalf("without Unicode the marks are %q, want %q", got, want)
	}
}

// A terminal too short for everything keeps the row that says the run is not
// strict: the composer gives way to it, down to a terminal of two rows.
func TestAShortTerminalKeepsTheNotStrictRow(t *testing.T) {
	for _, h := range []int{2, 3, 4, 6, 10} {
		a, _ := newTestApp()
		a.Current().Status.Posture, a.Current().Status.Grants = "strict", 1
		a.Dispatch(ActionResize{W: 80, H: h})
		rows := screen(a, 80, h)
		if !strings.Contains(strings.Join(rows, "\n"), "not a strict run") {
			t.Fatalf("a terminal of %d rows does not say the run is not strict:\n%s", h, strings.Join(rows, "\n"))
		}
		if last := rows[len(rows)-1]; !strings.Contains(last, "strict") {
			t.Fatalf("a terminal of %d rows lost the posture: %q", h, last)
		}
	}
}

// The keys of a focused transcript are many. They take their share of the
// footer and no more, so the model does not come and go with the focus; the
// sparkline is gated on the whole row's width, not on the status's share.
func TestTheFooterSharesItsRow(t *testing.T) {
	th := Dark()
	st := Status{Posture: "strict", Model: "claude-opus-5-5", GitBranch: "main", Busy: true}
	draw := func(w int, ctx Context, spark []float64) string {
		b := render.NewBuffer(w, 1)
		DrawFooter(b, b.Bounds(), th, st, ctx, 0, spark, true)
		return bufferString(b)
	}
	for _, ctx := range []Context{CtxPrompt, CtxScrollback} {
		got := draw(96, ctx, nil)
		if !strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "F1 more") {
			t.Fatalf("at 96 columns the footer reads %q", got)
		}
	}
	// The key that opens a folded result is on the row when the transcript has
	// the keys: every result opens folded, so it is the one a reader needs.
	if got := draw(96, CtxScrollback, nil); !strings.Contains(got, "fold") {
		t.Fatalf("a focused transcript's footer does not offer fold: %q", got)
	}
	spark := []float64{1, 4, 2, 8, 3, 6}
	bars := func(s string) bool { return strings.ContainsAny(s, "▁▂▃▄▅▆▇█") }
	if wide := draw(140, CtxPrompt, spark); !bars(wide) {
		t.Fatalf("a wide footer draws no sparkline while a turn runs: %q", wide)
	}
	if narrow := draw(100, CtxPrompt, spark); bars(narrow) {
		t.Fatalf("a footer under %d columns draws the sparkline: %q", sparkFrom, narrow)
	}
}
