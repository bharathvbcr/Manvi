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
	"unicode"
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
	// MaxAnswerBytes bounds the whole answer, checked before any of it is read.
	// A legitimate answer is a few kilobytes; a reader that took whatever a
	// server sent would let one runaway generation choose how much memory and
	// time the parser spends on it.
	MaxAnswerBytes = 64 << 10

	maxConfidenceRunes = 16
)

// verdictMarkerPrefix is the checkpoint critic's line prefix. An answer that
// carries it has quoted a verdict, which the packet never contains, so the
// reflector either saw something it was not given or is inventing a
// judgement in the critic's voice. Both are refused; see Packet.
const verdictMarkerPrefix = "MANVI-VERDICT"

// invisible reports runes that render as nothing, or rewrite what is around
// them, and so can hide or fake text: control characters other than newline
// and tab (an escape sequence in a cue is executed by the terminal that
// echoes it), format characters (zero-width joiners, bidi overrides, which
// split a marker or reorder a line) and private-use characters.
func invisible(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Co, r)
}

// ContainsMarker reports whether s carries the checkpoint critic's marker in
// any case, once the runes that could split it are removed. The packet builder
// and the parser both ask this one question, so the two sides cannot disagree
// about what counts.
func ContainsMarker(s string) bool {
	var b strings.Builder
	for _, r := range s {
		if !invisible(r) {
			b.WriteRune(r)
		}
	}
	return strings.Contains(strings.ToUpper(b.String()), verdictMarkerPrefix)
}

// fieldBuf is one experience field as it is read. The running rune count is
// what lets a field be refused the moment it is over its bound, instead of
// after the rest of the answer has been read into it.
type fieldBuf struct {
	text  strings.Builder
	runes int
	limit int
}

func (f *fieldBuf) add(s string) bool {
	f.runes += utf8.RuneCountInString(s)
	if f.runes > f.limit {
		return false
	}
	f.text.WriteString(s)
	return true
}

var fieldLimits = map[string]int{
	"cue": MaxCueRunes, "lesson": MaxLessonRunes, "evidence": MaxEvidenceRunes, "confidence": maxConfidenceRunes,
}

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
	if len(answer) > MaxAnswerBytes {
		return refuse("the answer is %d bytes, over the %d limit", len(answer), MaxAnswerBytes)
	}
	if !utf8.ValidString(answer) {
		return refuse("the answer is not valid UTF-8")
	}
	// A CRLF answer is the same answer; a lone carriage return is not, and is
	// refused below with the other control characters.
	answer = strings.ReplaceAll(answer, "\r\n", "\n")
	for _, r := range answer {
		if invisible(r) {
			return refuse("the answer contains the invisible or control character U+%04X", r)
		}
	}
	if ContainsMarker(answer) {
		return refuse("the answer contains %q, which belongs to the checkpoint critic and is never part of the evidence", verdictMarkerPrefix)
	}
	if strings.Contains(answer, PacketBegin) || strings.Contains(answer, PacketEnd) {
		return refuse("the answer contains an evidence-packet fence, so it is quoting its input rather than answering it")
	}

	var (
		where       = outside
		editBlocks  int
		expBlocks   int
		noEntry     int
		edits       []Edit
		fields      = map[string]*fieldBuf{}
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
			switch trimmed {
			case "</edits>":
				where = outside
			case "":
			case "<edits>", "<experience>":
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
				limit, known := fieldLimits[key]
				if !known {
					return refuse("unknown field %q in <experience>", key)
				}
				if _, dup := fields[key]; dup {
					return refuse("field %q appears twice", key)
				}
				buf := &fieldBuf{limit: limit}
				if !buf.add(rest) {
					return refuse("%s is over the %d limit", key, limit)
				}
				fields[key] = buf
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
			buf, ok := fields[currentKey]
			if !ok || buf == nil {
				return refuse("continuation of unknown field %q", currentKey)
			}
			if !buf.add(sep + trimmed) {
				return refuse("%s is over the %d limit", currentKey, buf.limit)
			}
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
	text := func(key string) string {
		if buf, ok := fields[key]; ok {
			return strings.TrimSpace(buf.text.String())
		}
		return ""
	}
	exp := &Experience{
		Cue:        text("cue"),
		Lesson:     text("lesson"),
		Evidence:   text("evidence"),
		Confidence: strings.ToLower(text("confidence")),
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
