package devcouncil

import (
	"slices"
	"testing"
)

func TestSubAgentNotesSplitOffTheEndInOrder(t *testing.T) {
	s := AppendSubAgentNote(AppendSubAgentNote("the answer [cites x]", "escalated from lite to heavy"), "re-ran on gemini/g")
	answer, notes := SplitSubAgentNotes(s)
	if answer != "the answer [cites x]" {
		t.Fatalf("answer = %q", answer)
	}
	if !slices.Equal(notes, []string{"escalated from lite to heavy", "re-ran on gemini/g"}) {
		t.Fatalf("notes = %q", notes)
	}
}

func TestANoteShapedStringInsideAnAnswerStaysInTheAnswer(t *testing.T) {
	s := "before\n\n[this sub-agent: not a note] and after"
	answer, notes := SplitSubAgentNotes(s)
	if answer != s || len(notes) != 0 {
		t.Fatalf("answer = %q notes = %q", answer, notes)
	}
	if a, n := SplitSubAgentNotes(""); a != "" || n != nil {
		t.Fatalf("empty = %q %q", a, n)
	}
}
