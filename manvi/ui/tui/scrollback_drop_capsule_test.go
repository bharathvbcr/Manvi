package tui

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// An expanded work capsule must stay the expanded one when older entries are
// dropped, whether or not an entry is selected: the operator can expand a
// capsule and then move focus so the selection is -1, and the bound still
// retires entries from the front.
func TestExpandedCapsuleSurvivesADropWithNothingSelected(t *testing.T) {
	s := NewScrollback()
	s.StackWork = true
	for i := 0; i < 3; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: strings.Repeat("p", 300)})
	}
	s.Append(ui.Event{Kind: ui.KindTurnStart, Text: "fix the bug"})
	s.Append(ui.Event{Kind: ui.KindReasoning, Text: "needle-in-thought: checking parser"})
	s.Append(ui.Event{Kind: ui.KindToolStart, Tool: "read_file", Arguments: []byte(`{"path":"a.go"}`)})
	s.Append(ui.Event{Kind: ui.KindToolResult, Tool: "read_file", Text: "needle-in-read\nline2\n"})
	s.Append(ui.Event{Kind: ui.KindText, Text: "final answer"})

	header := func() int {
		for i, e := range s.Entries() {
			if strings.HasPrefix(e.Event.Text, "needle-in-thought") {
				return i
			}
		}
		t.Fatal("capsule header entry is gone")
		return -1
	}
	before := header()
	s.expandedCapsules = map[int]bool{before: true}
	s.selected = -1
	s.hoverEntry = before

	s.MaxBytes = 600
	s.Append(ui.Event{Kind: ui.KindNotice, Text: strings.Repeat("n", 100)})
	if s.Dropped() == 0 {
		t.Fatal("the byte bound dropped nothing, so the test exercises nothing")
	}

	after := header()
	if len(s.expandedCapsules) != 1 || !s.expandedCapsules[after] {
		t.Errorf("expanded capsules = %v after a drop of %d; want exactly the header at %d", s.expandedCapsules, s.Dropped(), after)
	}
	if s.hoverEntry != after {
		t.Errorf("hoverEntry = %d after the drop, want it to follow its entry to %d", s.hoverEntry, after)
	}
}

// A hovered entry that is itself dropped is forgotten, not left pointing at a
// different entry.
func TestHoverIsResetWhenItsEntryIsDropped(t *testing.T) {
	s := NewScrollback()
	s.MaxEntries = 3
	for i := 0; i < 3; i++ {
		s.Append(ui.Event{Kind: ui.KindNotice, Text: "n"})
	}
	s.hoverEntry = 0
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "n"})
	if s.hoverEntry != -1 {
		t.Fatalf("hoverEntry = %d after its entry was dropped, want -1", s.hoverEntry)
	}
}
