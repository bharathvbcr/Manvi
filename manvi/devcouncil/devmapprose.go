package devcouncil

import (
	"encoding/json/v2"
	"regexp"
	"strings"
)

// WithheldCallSiteSample replaces a sample of call-site labels in devmap prose.
const WithheldCallSiteSample = "[call-site sample withheld]"

// withheldLabels is the marker the graph tool's tests look for.
const withheldLabels = WithheldCallSiteSample

var (
	// callSiteNote is the concept devmap's notes about unbound calls are
	// about. Its radius notes quote a sample of "receiver.callee" labels, and
	// a receiver is up to 64 characters of raw source text with no file
	// attached, so the sample cannot be attributed to a path the read gate
	// could judge.
	callSiteNote = regexp.MustCompile(`(?i)call\s*sites?`)
	// leadingCount is the count a note opens with. Only the opening count is
	// kept: the rest of the text follows the sample, where a receiver can put
	// anything, including a forged count.
	leadingCount = regexp.MustCompile(`(?i)^\s*((?:at least )?[0-9]{1,9}) (?:unresolved )?call sites?(?:\(s\))?`)
)

// noteClauseSeparator is what devmap joins its notes with (combine_reasons).
const noteClauseSeparator = "; "

// Digit-free renderings of the engine clauses that may follow a call-site
// clause. A kept clause would carry its numbers, and a receiver can write any
// clause it likes, so a clause is shown as its fixed marker and never as itself.
const (
	markerKeysCapped    = "[only some of the reached keys were checked against the unresolved ledger]"
	markerNamesCapped   = "[the unresolved ledger was checked for only some names]"
	markerNamesNotRead  = "[more names were not looked up (`unresolved_namesakes.names_not_checked`)]"
	markerFailedParse   = "[some files failed to parse, so a caller in them is missing from this answer]"
	markerNoExtractor   = "[some files are in a language with no call extractor in this build]"
	markerNoExtracted   = "[no call into or out of them was ever extracted, so an empty or short answer here is the extractor's absence, not the code's]"
	markerStartsSkipped = "[more start files were not checked for this]"
	markerLedgerGone    = "[the unresolved ledger could not be read at the generation these edges came from (it was pruned mid-query)]"
)

// samplesFurtherWithheld marks text that followed a call-site sample and could
// not be told apart from it.
const samplesFurtherWithheld = "[further text after the call-site sample withheld]"

// partialAnswerPrefix is what DevCouncil's devmap client puts in front of a
// walk_incomplete value (dc/devmap runQuery), so every graph tool sees the note
// with it. The prefix is the client's own text, never the receiver's, and it
// stands before the note's first clause, so it is set aside and put back.
const partialAnswerPrefix = "devmap says the answer is partial: "

// engineClause is a clause devmap writes after its call-site clause and the
// fixed, digit-free text it is shown as.
type engineClause struct {
	re     *regexp.Regexp
	marker string
}

var (
	// engineClauses are the completeness clauses devmap writes, matched whole
	// (the engine.rs and gaps.rs templates). A receiver can write any of them,
	// so a match proves nothing about who wrote it: the clause is shown as its
	// marker and never as itself, and what a forgery can say is only that a
	// check was partial, which is the safe direction. Their numbers and paths
	// are not shown.
	engineClauses = []engineClause{
		{regexp.MustCompile(`^only [0-9]{1,9} of [0-9]{1,9} reached key\(s\) were checked against the unresolved ledger$`), markerKeysCapped},
		{regexp.MustCompile(`^the unresolved ledger was checked for [0-9]{1,9} name\(s\) only$`), markerNamesCapped},
		{regexp.MustCompile("^[0-9]{1,9} more were not looked up \\(`unresolved_namesakes\\.names_not_checked`\\)$"), markerNamesNotRead},
		{regexp.MustCompile(`^[0-9]{1,9} file\(s\) failed to parse, so a caller in them is missing from this answer: `), markerFailedParse},
		{regexp.MustCompile(`in a language with no call extractor in this build$`), markerNoExtractor},
		{regexp.MustCompile(`^no call into or out of (?:it|them) was ever extracted, so an empty or short answer here is the extractor's absence, not the code's$`), markerNoExtracted},
		{regexp.MustCompile(`^[0-9]{1,9} more start file\(s\) were not checked for this$`), markerStartsSkipped},
		{regexp.MustCompile(`^the unresolved ledger could not be read at generation [0-9]{1,9}, the one these edges came from \(it was pruned mid-query\)$`), markerLedgerGone},
	}
	// fixedClauses are clauses with no number and no free text in them, kept as
	// they are: the markers above and devmap's own two number-free sentences.
	fixedClauses = map[string]bool{
		"they are listed in `unresolved_namesakes` as candidates to verify, not as callers": true,
		"callers the resolver could not bind are not listed — ask again":                    true,
		WithheldCallSiteSample: true, samplesFurtherWithheld: true,
		markerKeysCapped: true, markerNamesCapped: true, markerNamesNotRead: true,
		markerFailedParse: true, markerNoExtractor: true, markerNoExtracted: true,
		markerStartsSkipped: true, markerLedgerGone: true,
	}
	// cappedClause is how a clause that reports a capped check reads. A count
	// that sat in front of one is a floor.
	cappedClause = regexp.MustCompile(`(?i)\bonly\b.*\bchecked\b`)
)

// noteSegment is one clause of a note after sanitizing: kept text, or the
// call-site clause reduced to its count.
type noteSegment struct {
	text    string
	site    bool
	count   string
	capped  bool
	swallow bool
}

// WithholdCallSiteSamples is the one place devmap prose is made safe to show
// the model. devmap joins its notes with "; ", and one of them, the call-site
// clause, quotes up to a handful of receiver.callee labels: raw source text of
// no known file. That clause is replaced by its opening count and a fixed
// marker.
//
// The sample is the one part of a note that is free text, and a receiver can
// hold "; ", digits, a count and a copy of any clause devmap writes, so nothing
// after the opening count can be told from devmap's own text. The rule is
// therefore positional: the only number that reaches the reader is the one at
// the start of the first call-site clause, which sits where devmap wrote it,
// before any sample. Every later clause is shown as a fixed marker when it
// matches a template devmap writes (see engineClauses) and is dropped, with a
// mark, when it does not. A clause that spoke of a capped check and was dropped
// turns the count into "at least N". Text that does not speak of call sites is
// returned as it is. The function is idempotent: its output is made of counts
// it keeps and markers it keeps.
//
// Not covered: a clause before the first call-site clause is devmap's and is
// kept verbatim, including any file paths it lists.
func WithholdCallSiteSamples(text string) string {
	if !callSiteNote.MatchString(text) {
		return text
	}
	prefix := ""
	if rest, ok := strings.CutPrefix(text, partialAnswerPrefix); ok {
		prefix, text = partialAnswerPrefix, rest
	}
	segs := make([]noteSegment, 0, 4)
	siteAt := -1
	for _, clause := range strings.Split(text, noteClauseSeparator) {
		switch {
		case siteAt >= 0:
			if fixedClauses[clause] {
				segs = append(segs, noteSegment{text: clause})
				continue
			}
			if marker, ok := engineMarker(clause); ok {
				segs = append(segs, noteSegment{text: marker})
				continue
			}
			segs[siteAt].swallow = true
			segs[siteAt].capped = segs[siteAt].capped || cappedClause.MatchString(clause)
		case callSiteNote.MatchString(clause):
			seg := noteSegment{site: true}
			if m := leadingCount.FindStringSubmatch(clause); m != nil {
				seg.count = m[1]
			}
			segs = append(segs, seg)
			siteAt = len(segs) - 1
		default:
			segs = append(segs, noteSegment{text: clause})
		}
	}
	var out []string
	seen := map[string]bool{}
	add := func(clause string) {
		if fixedClauses[clause] {
			if seen[clause] {
				return
			}
			seen[clause] = true
		}
		out = append(out, clause)
	}
	for _, seg := range segs {
		if !seg.site {
			add(seg.text)
			continue
		}
		if seg.count != "" {
			count := seg.count
			if seg.capped && !strings.HasPrefix(strings.ToLower(count), "at least ") {
				count = "at least " + count
			}
			add(count + " call site(s) could not be bound")
		}
		add(WithheldCallSiteSample)
		if seg.swallow {
			add(samplesFurtherWithheld)
		}
	}
	return prefix + strings.Join(out, noteClauseSeparator)
}

func engineMarker(clause string) (string, bool) {
	for _, c := range engineClauses {
		if c.re.MatchString(clause) {
			return c.marker, true
		}
	}
	return "", false
}

// DevmapNoteText reads a devmap walk_incomplete or coverage_gap value as the
// text a reader may see: empty when the walk was complete (absent, null or
// false), the sanitized message when it is a string, and the sanitized JSON of
// any other shape. A value that is not JSON is sanitized as text.
func DevmapNoteText(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	switch text {
	case "", "null", "false":
		return ""
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return WithholdCallSiteSamples(text)
	}
	if message, ok := value.(string); ok {
		return WithholdCallSiteSamples(strings.TrimSpace(message))
	}
	return WithholdCallSiteSamples(sanitizeNoteValue(value))
}

// sanitizeNoteValue renders a non-string note as JSON text with every string in
// it sanitized, so a quoted sample cannot survive inside an object or array.
func sanitizeNoteValue(v any) string {
	var clean func(v any) any
	clean = func(v any) any {
		switch node := v.(type) {
		case string:
			return WithholdCallSiteSamples(node)
		case []any:
			for i := range node {
				node[i] = clean(node[i])
			}
		case map[string]any:
			for k := range node {
				node[k] = clean(node[k])
			}
		}
		return v
	}
	out, err := json.Marshal(clean(v), json.Deterministic(true))
	if err != nil {
		return WithheldCallSiteSample
	}
	return string(out)
}
