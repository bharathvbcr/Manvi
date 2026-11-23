package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// The tests in this file each pin a defect the transcript had, and use only
// what the package already exported to itself before the fix, so the file runs
// unchanged against the code as it was and fails there.

// rowsOf lays an entry out and returns its rows as text.
func rowsOf(e *Entry, th Theme, width int) []string {
	out := []string{}
	for _, l := range e.Lines(th, width) {
		out = append(out, l.Text())
	}
	return out
}

// TestStreamedThinkingIsOneEntry: a model's thinking arrives as chunks, as its
// reply does. Each chunk was an entry of its own, so a sentence broke across
// rows wherever the provider cut the stream.
func TestStreamedThinkingIsOneEntry(t *testing.T) {
	s := NewScrollback()
	for _, chunk := range []string{"The user is asking about ", "the paste reader. ", "I should read it."} {
		s.Append(ui.Event{Kind: ui.KindReasoning, Text: chunk})
	}
	if s.Len() != 1 {
		t.Fatalf("three chunks of one thought made %d entries, want 1", s.Len())
	}
	if got := s.Entries()[0].text(); got != "The user is asking about the paste reader. I should read it." {
		t.Fatalf("merged thought = %q", got)
	}

	// A sub-agent's thinking is not folded into this session's: the entry says
	// whose it is, and one entry cannot say two things.
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "a child's thought", Agent: "worker-1"})
	if s.Len() != 2 {
		t.Fatalf("a child's thought was merged into the parent's: %d entries", s.Len())
	}
}

// TestAListItemIsNotDrawnAsARemovedLine: a diff was recognised anywhere in a
// reply and ahead of a list, so "- item" was drawn as a line a diff removed —
// in the danger colour, with its inline code left as literal backticks.
func TestAListItemIsNotDrawnAsARemovedLine(t *testing.T) {
	th := Dark()
	th.Unicode = true
	e := &Entry{Event: ui.Event{Kind: ui.KindText, Text: "- the tail sat in `pending`\n+ and one more thing"}}
	lines := e.Lines(th, 80)
	text := lineText(lines)
	if !strings.Contains(text, "• the tail sat in pending") {
		t.Fatalf("a list item was not drawn as one:\n%s", text)
	}
	if strings.Contains(text, "`") {
		t.Fatalf("inline code in a list item kept its backticks:\n%s", text)
	}
	for _, l := range lines {
		for _, span := range l {
			if strings.TrimSpace(span.Text) != "" && (span.Style.Fg == th.Danger || span.Style.Fg == th.Success) {
				t.Fatalf("%q in a reply is drawn in a diff colour", span.Text)
			}
		}
	}
}

// TestALongQuestionWrapsInsteadOfBeingCut: what the operator asked was one row,
// cut at the pane's edge.
func TestALongQuestionWrapsInsteadOfBeingCut(t *testing.T) {
	question := "why does the paste reader drop the last chunk when the terminal closes in the middle of a paste"
	e := &Entry{Event: ui.Event{Kind: ui.KindTurnStart, Text: question}}
	s := NewScrollback()
	s.entries = append(s.entries, e)
	b := render.NewBuffer(40, 12)
	s.Draw(b, b.Bounds(), Dark(), false)
	frame := strings.Join(strings.Fields(bufferString(b)), " ")
	if !strings.Contains(frame, "in the middle of a paste") {
		t.Fatalf("the end of the question is not on screen:\n%s", bufferString(b))
	}
	if strings.Contains(frame, "…") {
		t.Fatalf("the question was cut with an ellipsis:\n%s", bufferString(b))
	}
}

// TestCodeKeepsItsIndentation: the buffer drops a tab as it drops every control
// character, so tab-indented code — every Go file — was drawn flush left, in a
// reply's code block and in what a tool printed alike.
func TestCodeKeepsItsIndentation(t *testing.T) {
	th := Dark()
	reply := &Entry{Event: ui.Event{Kind: ui.KindText, Text: "```go\nif err != nil {\n\treturn err\n}\n```"}}
	result := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: "func f() {\n\treturn\n}"}}
	for name, e := range map[string]*Entry{"a code block": reply, "a tool result": result} {
		s := NewScrollback()
		s.entries = append(s.entries, e)
		b := render.NewBuffer(60, 8)
		s.Draw(b, b.Bounds(), th, false)
		if frame := bufferString(b); !strings.Contains(frame, "    return") {
			t.Fatalf("%s lost its indentation:\n%s", name, frame)
		}
	}
}

// TestANarrowStatusBarKeepsTheModel: the bar drew its counters in full and cut
// the identity to what was left, so at sixty columns the model was gone and an
// ellipsis stood where it had been.
func TestANarrowStatusBarKeepsTheModel(t *testing.T) {
	th := Dark()
	th.Unicode = true
	st := Status{
		Posture: "strict", Model: "claude-opus-5-5",
		InputTokens: 18234, OutputTokens: 912, Grants: 1,
	}
	b := render.NewBuffer(60, 1)
	DrawStatusBar(b, b.Bounds(), th, st, 0, nil, false)
	out := bufferString(b)
	for _, want := range []string{"strict", "claude-opus-5-5", "1 granted"} {
		if !strings.Contains(out, want) {
			t.Fatalf("a sixty-column bar dropped %q: %q", want, out)
		}
	}
	if strings.Contains(out, "…") {
		t.Fatalf("a segment was cut rather than dropped whole: %q", out)
	}
}

// TestATurnEndDrawsNothing: a turn's end reached the transcript as an entry with
// no body, which is a bullet on a row of its own after every turn.
func TestATurnEndDrawsNothing(t *testing.T) {
	s := NewScrollback()
	s.Append(ui.Event{Kind: ui.KindText, Text: "done"})
	s.Append(ui.Event{Kind: ui.KindTurnEnd})
	if s.Len() != 1 {
		t.Fatalf("a turn end became transcript entry %d", s.Len())
	}
}

// TestTestOutputIsNotADiffHeader: "--- FAIL: TestX" opens with what a unified
// diff's header opens with, and was drawn as one — bold, on the code surface —
// in the middle of a test run's output.
func TestTestOutputIsNotADiffHeader(t *testing.T) {
	th := Dark()
	e := &Entry{Event: ui.Event{Kind: ui.KindToolResult, IsError: true, Text: "--- FAIL: TestPaste (0.01s)\n    reader_test.go:88: tail lost\nFAIL"}}
	for _, l := range e.Lines(th, 80) {
		for _, span := range l {
			if strings.Contains(span.Text, "FAIL: TestPaste") && (span.Style.Attrs&render.Bold != 0 || span.Style.Bg == th.BgInset) {
				t.Fatalf("a test failure line is styled as a diff header: %+v", span.Style)
			}
		}
	}
}

// TestTheDashboardDoesNotSayJustNowAgo: the age of a session active within the
// last second is already a phrase, and took the suffix every other age takes.
func TestTheDashboardDoesNotSayJustNowAgo(t *testing.T) {
	a, _ := newTestApp()
	mustCurrent(a).Apply(ui.Event{Kind: ui.KindNotice, Text: "something happened"})
	// Dated ahead of the clock, so the age is under a second however long the
	// test takes to reach the draw.
	a.Current().lastActivity = time.Now().Add(time.Hour)
	key(a, "ctrl+g")
	rows := screen(a, 100, 24)
	if !contains2(rows, "just now") {
		t.Fatalf("the dashboard does not show the session's age:\n%s", strings.Join(rows, "\n"))
	}
	if contains2(rows, "just now ago") {
		t.Fatalf("the dashboard says %q", "just now ago")
	}
}

// TestAShellCommentIsNotAHeading: what a tool printed was read as markdown, so a
// "# comment" in a script it read was drawn as a heading with the "#" removed.
func TestAShellCommentIsNotAHeading(t *testing.T) {
	e := &Entry{Event: ui.Event{Kind: ui.KindToolResult, Text: "# install the hook\n* matches everything"}}
	got := strings.Join(rowsOf(e, Dark(), 80), "\n")
	if !strings.Contains(got, "# install the hook") || !strings.Contains(got, "* matches everything") {
		t.Fatalf("a tool's output was rewritten as markdown:\n%s", got)
	}
}
