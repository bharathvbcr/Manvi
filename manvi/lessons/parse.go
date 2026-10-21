// Package lessons is the lesson library: what `manvi reflect` learns from one
// session, kept where a person can read it and promote it.
//
// Three things live here and they are one contract. The packet is the evidence
// the reflector is shown, built from the session log alone. The parser is how
// the reflector's answer is read. The store is where an accepted answer lands.
// They are in one package because each is only as honest as the others: a
// packet that leaked the critic's verdict would bias an entry the parser could
// not see was biased, and a parser that tolerated a half-formed entry would
// hand the store something it could only write down as fact.
package lessons

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The reader below is the lesson-shaped twin of devcouncil.ParseVerdict, and it
// keeps that file's one rule: it fails closed. A reflector answers in prose and
// in one machine-readable block, and anything this reader cannot take whole it
// refuses, because the alternatives are all inventions — keeping the first of
// two entries is a tie-break nobody specified, filling a missing field is a
// guess that will be read later as the reflector's finding, and reading an
// unknown verdict as "no observed effect" turns a model's confusion into a
// claim about the harness.
//
// Refusal stores nothing. That asymmetry is the design: the cost of refusing a
// good entry is one more run of `manvi reflect`, and the cost of keeping a bad
// one is a lesson in the library that someone promotes into AGENTS.md.

// Verdict is how one harness change is attributed.
type Verdict string

// The four attributions, and no others. A verdict is compared whole: "helped a
// lot" is not "helped", and a reader that matched the prefix would store what
// the reflector wrote rather than what it was allowed to say.
const (
	VerdictHelped     Verdict = "helped"
	VerdictHurt       Verdict = "hurt"
	VerdictNoEffect   Verdict = "no observed effect"
	VerdictConfounded Verdict = "confounded"
)

func (v Verdict) valid() bool {
	switch v {
	case VerdictHelped, VerdictHurt, VerdictNoEffect, VerdictConfounded:
		return true
	}
	return false
}

// NoEntryLine is the whole of an answer that has no lesson to record.
const NoEntryLine = "NO_ENTRY"

// Bounds on what one entry may say. Over the bound is a refusal, not a
// truncation: a lesson cut mid-sentence reads as a complete one.
const (
	MaxCueRunes      = 240
	MaxLessonRunes   = 1200
	MaxEvidenceRunes = 1200
	MaxEditRows      = 24
	MaxEditRunes     = 300
)

// verdictMarkerPrefix is the checkpoint critic's line prefix. An answer that
// carries it has quoted a verdict, which the packet never contains, so the
// reflector either saw something it was not given or is inventing a
// judgement in the critic's voice. Both are refused; see Packet.
const verdictMarkerPrefix = "MANVI-VERDICT"

// Edit is one harness change and how the reflector attributes the outcome to it.
type Edit struct {
	Change  string
	Verdict Verdict
	Note    string
}

// Experience is the one lesson a session may yield.
type Experience struct {
	Cue        string
	Lesson     string
	Evidence   string
	Confidence string
}

// Result is a reading of the reflector's answer. Exactly one of NoEntry and
// Experience is set on success; on error neither is, so a caller that ignores
// the error still holds nothing it could store.
type Result struct {
	Edits      []Edit
	NoEntry    bool
	Experience *Experience
}

// ErrRefused marks every refusal, so a caller can tell "the answer was not
// acceptable" from "the reader broke".
var ErrRefused = errors.New("the reflector's answer was refused")

func refuse(format string, args ...any) (Result, error) {
	return Result{}, fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

type section int

const (
	outside section = iota
	inEdits
	inExperience
)

// Parse reads a reflector's answer.
func Parse(answer string) (Result, error) {
	if strings.Contains(answer, verdictMarkerPrefix) {
		return refuse("the answer contains %q, which belongs to the checkpoint critic and is never part of the evidence", verdictMarkerPrefix)
	}
	if strings.ContainsRune(answer, 0) {
		return refuse("the answer contains a NUL byte")
	}

	var (
		where       = outside
		editBlocks  int
		expBlocks   int
		noEntry     int
		edits       []Edit
		fields      = map[string]string{}
		currentKey  string
		sawField    bool
		pendingBlnk bool
	)

	for line := range strings.Lines(answer) {
		trimmed := strings.TrimSpace(line)
		switch where {
		case outside:
			switch trimmed {
			case "<edits>":
				editBlocks++
				if editBlocks > 1 {
					return refuse("more than one <edits> block")
				}
				where = inEdits
			case "<experience>":
				expBlocks++
				if expBlocks > 1 {
					return refuse("more than one <experience> block: at most one lesson per session")
				}
				where = inExperience
				currentKey, sawField, pendingBlnk = "", false, false
			case NoEntryLine:
				noEntry++
			case "</edits>", "</experience>":
				return refuse("%s with no opening tag", trimmed)
			}
		case inEdits:
			switch {
			case trimmed == "</edits>":
				where = outside
			case trimmed == "":
			case trimmed == "<edits>" || trimmed == "<experience>":
				return refuse("%s inside <edits>: blocks do not nest", trimmed)
			default:
				row, err := parseEditRow(trimmed)
				if err != nil {
					return Result{}, err
				}
				if len(edits) >= MaxEditRows {
					return refuse("more than %d edit rows", MaxEditRows)
				}
				edits = append(edits, row)
			}
		case inExperience:
			if trimmed == "</experience>" {
				where = outside
				continue
			}
			if trimmed == "<edits>" || trimmed == "<experience>" {
				return refuse("%s inside <experience>: blocks do not nest", trimmed)
			}
			if key, rest, ok := fieldLine(trimmed); ok {
				switch key {
				case "cue", "lesson", "evidence", "confidence":
				default:
					return refuse("unknown field %q in <experience>", key)
				}
				if _, dup := fields[key]; dup {
					return refuse("field %q appears twice", key)
				}
				fields[key] = rest
				currentKey, sawField, pendingBlnk = key, true, false
				continue
			}
			if trimmed == "" {
				pendingBlnk = sawField
				continue
			}
			if !sawField {
				return refuse("text before the first field in <experience>")
			}
			if currentKey == "cue" || currentKey == "confidence" {
				return refuse("%s must be a single line", currentKey)
			}
			sep := "\n"
			if pendingBlnk {
				sep = "\n\n"
			}
			fields[currentKey] += sep + trimmed
			pendingBlnk = false
		}
	}

	switch where {
	case inEdits:
		return refuse("<edits> is not closed")
	case inExperience:
		return refuse("<experience> is not closed")
	}

	switch {
	case noEntry > 1:
		return refuse("%s appears more than once", NoEntryLine)
	case noEntry == 1 && expBlocks == 1:
		return refuse("the answer holds both an <experience> block and %s", NoEntryLine)
	case noEntry == 1:
		return Result{Edits: edits, NoEntry: true}, nil
	case expBlocks == 0:
		return refuse("no <experience> block and no %s line", NoEntryLine)
	}

	if editBlocks == 0 || len(edits) == 0 {
		return refuse("an entry needs an <edits> block with at least one attributed row")
	}
	exp := &Experience{
		Cue:        strings.TrimSpace(fields["cue"]),
		Lesson:     strings.TrimSpace(fields["lesson"]),
		Evidence:   strings.TrimSpace(fields["evidence"]),
		Confidence: strings.ToLower(strings.TrimSpace(fields["confidence"])),
	}
	for _, f := range []struct {
		name, text string
		limit      int
	}{
		{"cue", exp.Cue, MaxCueRunes},
		{"lesson", exp.Lesson, MaxLessonRunes},
		{"evidence", exp.Evidence, MaxEvidenceRunes},
	} {
		if f.text == "" {
			return refuse("%s is missing or empty", f.name)
		}
		if n := utf8.RuneCountInString(f.text); n > f.limit {
			return refuse("%s is %d characters, over the %d limit", f.name, n, f.limit)
		}
	}
	switch exp.Confidence {
	case "low", "medium", "high":
	case "":
		return refuse("confidence is missing or empty")
	default:
		return refuse("confidence %q is not low, medium or high", exp.Confidence)
	}
	return Result{Edits: edits, Experience: exp}, nil
}

// fieldLine reads "key: value" where key is a bare lowercase word. A line
// whose head is anything else — a URL, "e.g.:", a sentence with a colon — is
// continuation text, not a field.
func fieldLine(line string) (key, rest string, ok bool) {
	head, tail, found := strings.Cut(line, ":")
	if !found || head == "" || len(head) > 20 {
		return "", "", false
	}
	for _, r := range head {
		if (r < 'a' || r > 'z') && r != '_' {
			return "", "", false
		}
	}
	return head, strings.TrimSpace(tail), true
}

// parseEditRow reads "- <change> | <verdict> [| <note>]".
func parseEditRow(line string) (Edit, error) {
	body, ok := strings.CutPrefix(line, "- ")
	if !ok {
		_, err := refuse("an <edits> row must begin with \"- \": %q", clip(line))
		return Edit{}, err
	}
	parts := strings.Split(body, "|")
	if len(parts) < 2 {
		_, err := refuse("an <edits> row needs \"<change> | <verdict>\": %q", clip(line))
		return Edit{}, err
	}
	change := strings.TrimSpace(parts[0])
	verdict := Verdict(strings.Join(strings.Fields(strings.ToLower(parts[1])), " "))
	if change == "" {
		_, err := refuse("an <edits> row names no change: %q", clip(line))
		return Edit{}, err
	}
	if !verdict.valid() {
		_, err := refuse("unknown verdict %q (want helped, hurt, no observed effect or confounded)", clip(parts[1]))
		return Edit{}, err
	}
	note := strings.TrimSpace(strings.Join(parts[2:], "|"))
	if utf8.RuneCountInString(change)+utf8.RuneCountInString(note) > MaxEditRunes {
		_, err := refuse("an <edits> row is longer than %d characters", MaxEditRunes)
		return Edit{}, err
	}
	return Edit{Change: change, Verdict: verdict, Note: note}, nil
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}
