package tui

import (
	"strings"
	"testing"
)

func TestBackspaceOverAMultiByteCharacter(t *testing.T) {
	// Editing on a string rather than runes deletes half a character here, and
	// the buffer becomes invalid UTF-8 from that point on.
	p := NewPrompt()
	p.SetValue("héllo漢")
	p.Backspace()
	if got := p.Value(); got != "héllo" {
		t.Fatalf("got %q", got)
	}
	p.Backspace()
	p.Backspace()
	if got := p.Value(); got != "hél" {
		t.Fatalf("got %q", got)
	}
}

func TestWordMotionAndDeletion(t *testing.T) {
	p := NewPrompt()
	p.SetValue("fix the broken parser")
	p.DeleteWord()
	if got := p.Value(); got != "fix the broken " {
		t.Fatalf("ctrl+w left %q", got)
	}
	p.MoveWordLeft()
	p.MoveWordLeft()
	p.DeleteToEnd()
	if got := p.Value(); got != "fix " {
		t.Fatalf("ctrl+k left %q", got)
	}
}

func TestDeleteToStartIsLineScopedNotBufferScoped(t *testing.T) {
	p := NewPrompt()
	p.SetValue("first line\nsecond line")
	p.MoveBufferEnd()
	p.DeleteToStart()
	if got := p.Value(); got != "first line\n" {
		t.Fatalf("ctrl+u crossed the line boundary: %q", got)
	}
}

func TestHistoryRestoresTheDraft(t *testing.T) {
	// Walking off the end of history must restore what was being typed, not
	// clear it.
	p := NewPrompt()
	p.SetValue("first")
	p.Submit()
	p.SetValue("second")
	p.Submit()

	p.SetValue("a draft in progress")
	p.HistoryPrev()
	if got := p.Value(); got != "second" {
		t.Fatalf("got %q", got)
	}
	p.HistoryPrev()
	if got := p.Value(); got != "first" {
		t.Fatalf("got %q", got)
	}
	p.HistoryNext()
	p.HistoryNext()
	if got := p.Value(); got != "a draft in progress" {
		t.Fatalf("the draft was not restored: %q", got)
	}
}

func TestHistorySkipsConsecutiveDuplicates(t *testing.T) {
	p := NewPrompt()
	for i := 0; i < 5; i++ {
		p.SetValue("retry")
		p.Submit()
	}
	if got := len(p.History()); got != 1 {
		t.Fatalf("history holds %d entries, want 1", got)
	}
}

func TestTriggerDetection(t *testing.T) {
	cases := []struct {
		text string
		kind rune
		want string
	}{
		{"/", '/', ""},
		{"/che", '/', "che"},
		{"/check src", 0, ""},          // a space ends the command name
		{"edit src/main.go", 0, ""},    // a slash inside a path is not a trigger
		{"see @src/ma", '@', "src/ma"}, // a file reference
		{"mail@example.com", 0, ""},    // an address is not a file reference
		{"@top", '@', "top"},           // at the start of the buffer
	}
	for _, c := range cases {
		p := NewPrompt()
		p.SetValue(c.text)
		got := p.ActiveTrigger()
		if got.Kind != c.kind || got.Query != c.want {
			t.Errorf("%q: trigger = %q/%q, want %q/%q", c.text,
				string(got.Kind), got.Query, string(c.kind), c.want)
		}
	}
}

func TestApplyCompletionReplacesOnlyTheTrigger(t *testing.T) {
	p := NewPrompt()
	p.SetValue("look at @src/ma")
	trigger := p.ActiveTrigger()
	p.ApplyCompletion(trigger, "src/main.go")
	if got := p.Value(); got != "look at @src/main.go " {
		t.Fatalf("got %q", got)
	}
}

func TestLayoutWrapsAndTracksTheCaret(t *testing.T) {
	p := NewPrompt()
	p.SetValue(strings.Repeat("a", 25))
	lines, row, col := p.layout(10)
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3", len(lines))
	}
	if row != 2 || col != 5 {
		t.Fatalf("caret at %d,%d, want 2,5", row, col)
	}
	for i, l := range lines {
		if len(l) > 10 {
			t.Fatalf("line %d is %q", i, l)
		}
	}
}

func TestLayoutCountsColumnsNotRunes(t *testing.T) {
	// A composer sized by rune count overflows its box with CJK text, and the
	// overflow wraps in the terminal rather than in the layout.
	p := NewPrompt()
	p.SetValue(strings.Repeat("漢", 8))
	lines, _, _ := p.layout(10)
	if len(lines) != 2 {
		t.Fatalf("%d lines, want 2 — eight wide runes are 16 columns", len(lines))
	}
}

func TestSubmitTrimsAndClears(t *testing.T) {
	p := NewPrompt()
	p.SetValue("  text with trailing space   \n")
	if got := p.Submit(); got != "  text with trailing space" {
		t.Fatalf("got %q", got)
	}
	if !p.Empty() {
		t.Fatal("submit did not clear the buffer")
	}
	if got := p.Submit(); got != "" {
		t.Fatalf("submitting an empty prompt returned %q", got)
	}
}
