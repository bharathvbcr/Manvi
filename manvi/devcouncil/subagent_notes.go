package devcouncil

import (
	"slices"
	"strings"
)

// subAgentNoteOpen opens a note the harness appends to a child's summary about
// how the child ran (a step ceiling, an escalation declined, a failover)
// rather than what it found.
//
// The notes travel in the summary because the dispatching model is their only
// reader. A consumer that hands one child's answer to another model, as a
// debate does, must not hand on the notes too: they name tiers, team members
// and providers, which is exactly what a blind comparison withholds.
const subAgentNoteOpen = "\n\n[this sub-agent: "

// AppendSubAgentNote adds one harness note to a child's summary. Every note
// goes through here so SplitSubAgentNotes can find them all.
func AppendSubAgentNote(summary, note string) string {
	return summary + subAgentNoteOpen + note + "]"
}

// SplitSubAgentNotes separates a child's own answer from the harness notes
// appended after it, in the order they were appended. Notes are only ever
// appended, so they are read off the end: a note-shaped string in the middle
// of an answer is the child's text and stays in the answer.
func SplitSubAgentNotes(summary string) (answer string, notes []string) {
	answer = summary
	for strings.HasSuffix(answer, "]") {
		i := strings.LastIndex(answer, subAgentNoteOpen)
		if i < 0 {
			break
		}
		notes = append(notes, answer[i+len(subAgentNoteOpen):len(answer)-1])
		answer = answer[:i]
	}
	slices.Reverse(notes)
	return answer, notes
}
