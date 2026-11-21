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

// samplesFurtherWithheld marks text that followed a call-site sample and could
// not be told apart from it.
const samplesFurtherWithheld = "[further text after the call-site sample withheld]"

var (
	// completenessClauses are the devmap clauses that carry no sample and
	// matter to a reader: the caps on how much of the unresolved ledger was
	// checked. They are kept after a call-site clause only when the whole
	// clause matches a template exactly, so a receiver that imitates one can
	// say nothing a reader did not already know.
	completenessClauses = []*regexp.Regexp{
		regexp.MustCompile(`^only [0-9]{1,9} of [0-9]{1,9} reached key\(s\) were checked against the unresolved ledger$`),
		regexp.MustCompile(`^the unresolved ledger was checked for [0-9]{1,9} name\(s\) only$`),
		regexp.MustCompile("^[0-9]{1,9} more were not looked up \\(`unresolved_namesakes\\.names_not_checked`\\)$"),
		regexp.MustCompile("^they are listed in `unresolved_namesakes` as candidates to verify, not as callers$"),
	}
	// cappedClause is how a clause that reports a capped check reads. A count
	// that sat in front of one is a floor.
	cappedClause = regexp.MustCompile(`(?i)\bonly\b.*\bchecked\b`)
)

// noteSegment is one clause of a note after sanitizing: kept text, or a
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
// marker, and every other clause is kept as it was written, so a depth cap or
// a capped ledger check still reaches the reader.
//
// The sample is the one part of a note that is free text, so the clause cannot
// be delimited by trusting its own punctuation: a receiver can hold "; ". From
// the opening count, every following clause is dropped unless it matches a
// completeness template in full, and a drop is marked. A clause that spoke of
// a capped check and was dropped turns the count into "at least N". Text that
// does not speak of call sites is returned as it is.
func WithholdCallSiteSamples(text string) string {
	if !callSiteNote.MatchString(text) {
		return text
	}
	var segs []noteSegment
	inSample := false
	for _, clause := range strings.Split(text, noteClauseSeparator) {
		m := leadingCount.FindStringSubmatch(clause)
		switch {
		case m != nil && callSiteNote.MatchString(clause):
			segs = append(segs, noteSegment{site: true, count: m[1]})
			inSample = true
		case inSample:
			if isCompletenessClause(clause) {
				segs = append(segs, noteSegment{text: clause})
				continue
			}
			for i := len(segs) - 1; i >= 0; i-- {
				if segs[i].site {
					segs[i].swallow = true
					segs[i].capped = segs[i].capped || cappedClause.MatchString(clause)
					break
				}
			}
		case callSiteNote.MatchString(clause):
			segs = append(segs, noteSegment{site: true})
			inSample = true
		default:
			segs = append(segs, noteSegment{text: clause})
		}
	}
	var out []string
	for _, seg := range segs {
		if !seg.site {
			out = append(out, seg.text)
			continue
		}
		if seg.count != "" {
			count := seg.count
			if seg.capped && !strings.HasPrefix(strings.ToLower(count), "at least ") {
				count = "at least " + count
			}
			out = append(out, count+" call site(s) could not be bound")
		}
		out = append(out, WithheldCallSiteSample)
		if seg.swallow {
			out = append(out, samplesFurtherWithheld)
		}
	}
	return strings.Join(out, noteClauseSeparator)
}

func isCompletenessClause(clause string) bool {
	for _, re := range completenessClauses {
		if re.MatchString(clause) {
			return true
		}
	}
	return false
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
