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

// WithholdCallSiteSamples is the one place devmap prose is made safe to show
// the model. Text that speaks of call sites is replaced whole by its opening
// count and a fixed marker; text that does not is returned as it is. It does
// not parse the note: a receiver can contain any delimiter a parser would
// trust, so nothing after the opening count is read, and a note that opens with
// no count becomes the marker alone.
func WithholdCallSiteSamples(text string) string {
	if !callSiteNote.MatchString(text) {
		return text
	}
	if m := leadingCount.FindStringSubmatch(text); m != nil {
		return m[1] + " call site(s) could not be bound; " + WithheldCallSiteSample
	}
	return WithheldCallSiteSample
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
