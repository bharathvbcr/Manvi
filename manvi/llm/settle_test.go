package llm

import (
	"fmt"
	"strings"
	"testing"
)

func TestSettleCallIsOneRuleForWhatACompleteCallIs(t *testing.T) {
	for _, c := range []struct {
		name string
		e    CallEvidence
		want CallVerdict
		args string
		say  string
	}{
		{"closed object", CallEvidence{ID: "i", Name: "n", Arguments: "{\"a\":1}", Closed: true, Framing: FramingRequired}, CallComplete, "{\"a\":1}", ""},
		{"closed, no bytes, is {}", CallEvidence{ID: "i", Name: "n", Closed: true, Framing: FramingRequired}, CallComplete, "{}", ""},
		{"closed, no bytes, at the limit, is still {}", CallEvidence{ID: "i", Name: "n", Closed: true, Framing: FramingRequired, Stop: StopMaxTokens}, CallComplete, "{}", ""},
		{"unframed wire, no bytes, is {}", CallEvidence{ID: "i", Name: "n", Framing: FramingNone}, CallComplete, "{}", ""},
		{"unframed wire, no bytes, at the limit, is cut", CallEvidence{ID: "i", Name: "n", Framing: FramingNone, Stop: StopMaxTokens}, CallCutByLimit, "", "output limit after 0 bytes"},
		{"optional framing accepts a whole unclosed object", CallEvidence{ID: "i", Name: "n", Arguments: "{\"a\":1}", Framing: FramingOptional}, CallComplete, "{\"a\":1}", ""},
		{"optional framing, unclosed, no bytes", CallEvidence{ID: "i", Name: "n", Framing: FramingOptional}, CallUnusable, "", "never closed"},
		{"required framing, unclosed whole object", CallEvidence{ID: "i", Name: "n", Arguments: "{}", Framing: FramingRequired}, CallUnusable, "", "never closed"},
		{"required framing, unclosed whole object, at the limit", CallEvidence{ID: "i", Name: "n", Arguments: "{}", Framing: FramingRequired, Stop: StopMaxTokens}, CallCutByLimit, "", "output limit"},
		{"array", CallEvidence{ID: "i", Name: "n", Arguments: "[1]", Closed: true}, CallUnusable, "", "not a JSON object"},
		{"string", CallEvidence{ID: "i", Name: "n", Arguments: "\"x\"", Closed: true}, CallUnusable, "", "not a JSON object"},
		{"cut object", CallEvidence{ID: "i", Name: "n", Arguments: "{\"a\":", Closed: true}, CallUnusable, "", "incomplete arguments"},
		{"cut object at the limit", CallEvidence{ID: "i", Name: "n", Arguments: "{\"a\":", Closed: true, Stop: StopMaxTokens}, CallCutByLimit, "", "output limit"},
		{"no name", CallEvidence{ID: "i", Arguments: "{}", Closed: true}, CallUnusable, "", "no function name"},
		{"blank name at the limit stays unusable", CallEvidence{ID: "i", Name: "  ", Closed: true, Stop: StopMaxTokens}, CallUnusable, "", "no function name"},
		{"no id", CallEvidence{Name: "n", Arguments: "{}", Closed: true}, CallUnusable, "", "no id"},
		{"no id on a wire that answers by name", CallEvidence{Name: "n", Arguments: "{}", Closed: true, IDOptional: true}, CallComplete, "{}", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := SettleCall(c.e)
			if got.Verdict != c.want {
				t.Fatalf("verdict = %d (%s), want %d", got.Verdict, got.Reason, c.want)
			}
			if c.want == CallComplete && string(got.Arguments) != c.args {
				t.Fatalf("arguments = %q, want %q", got.Arguments, c.args)
			}
			if c.want != CallComplete && (len(got.Arguments) != 0 || !strings.Contains(got.Reason, c.say)) {
				t.Fatalf("arguments = %q, reason = %q, want no arguments and a reason containing %q", got.Arguments, got.Reason, c.say)
			}
		})
	}
}

func TestNoteUnknownIsSortedDistinctAndBounded(t *testing.T) {
	var d DecodingReport
	if !d.Clean() {
		t.Fatal("a zero report is not clean")
	}
	d.NoteUnknown("b")
	d.NoteUnknown("a")
	d.NoteUnknown("b")
	if got := strings.Join(d.UnknownKinds, ","); got != "a,b" || d.Clean() {
		t.Fatalf("kinds = %q, clean = %v", got, d.Clean())
	}
	for i := 0; i < 40; i++ {
		d.NoteUnknown(fmt.Sprintf("kind_%02d", i))
	}
	if len(d.UnknownKinds) != maxUnknownKinds || d.UnknownKindsMore != 2+40-maxUnknownKinds {
		t.Fatalf("listed %d and counted %d more; want %d listed and the rest counted", len(d.UnknownKinds), d.UnknownKindsMore, maxUnknownKinds)
	}
	d.NoteUnknown(strings.Repeat("x", 500))
	for _, k := range d.UnknownKinds {
		if len(k) > maxUnknownKindLen {
			t.Fatalf("kind of %d bytes kept", len(k))
		}
	}
}
