package llm

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func advFramings() map[string]CallFraming {
	return map[string]CallFraming{"required": FramingRequired, "optional": FramingOptional, "none": FramingNone}
}

// Probe 2 at the rule itself, on every framing, closed and (where the wire
// allows) unclosed.
func TestAdvB31SettleCallEdgeArguments(t *testing.T) {
	huge := "{\"k\":\"" + strings.Repeat("A", 1<<20) + "\"}"
	cases := []struct {
		name string
		args string
		want CallVerdict
	}{
		{"whitespace-only", " \n\t ", CallComplete},
		{"object trailing whitespace", "{}  \n\t", CallComplete},
		{"two objects", "{} {}", CallUnusable},
		{"BOM object", "\ufeff{}", CallUnusable},
		{"null", "null", CallUnusable},
		{"exponent number", "1e5", CallUnusable},
		{"1 MB object", huge, CallComplete},
		{"string", "\"x\"", CallUnusable},
		{"array", "[{}]", CallUnusable},
	}
	for fname, f := range advFramings() {
		for _, c := range cases {
			t.Run(fname+"/"+c.name, func(t *testing.T) {
				got := SettleCall(CallEvidence{ID: "c1", Name: "bash", Arguments: c.args, Closed: true, Framing: f, Stop: StopToolUse})
				if got.Verdict != c.want {
					t.Fatalf("verdict %d (%s), want %d", got.Verdict, got.Reason, c.want)
				}
				if got.Verdict == CallComplete {
					var m map[string]json.RawMessage
					if err := json.Unmarshal(got.Arguments, &m); err != nil {
						t.Fatalf("complete but arguments are not an object: %q", got.Arguments)
					}
				}
			})
		}
	}
}

// A whole, valid, closed JSON value that is not an object is not "cut off by
// the output limit": the bytes are all there. Reporting it as a cut tells the
// model to shorten something that was not too long.
func TestAdvB31SettleCallWholeNonObjectUnderLimitIsNotACut(t *testing.T) {
	for _, args := range []string{"null", "\"x\"", "[1]", "1e5"} {
		got := SettleCall(CallEvidence{ID: "c1", Name: "bash", Arguments: args, Closed: true, Framing: FramingRequired, Stop: StopMaxTokens})
		if got.Verdict == CallCutByLimit {
			t.Errorf("%q: closed and valid JSON, classified as cut by the output limit: %s", args, got.Reason)
		}
	}
}

// The reason travels to the model (MalformedCall) and to logs. It embeds the
// arguments, which are unbounded wire bytes.
func TestAdvB31SettleCallReasonIsBounded(t *testing.T) {
	const bound = 4 << 10
	big := strings.Repeat("x", 1<<20)
	for fname, f := range advFramings() {
		for _, args := range []string{big, "\"" + big + "\"", "{" + big} {
			got := SettleCall(CallEvidence{ID: "c1", Name: "bash", Arguments: args, Closed: true, Framing: f, Stop: StopToolUse})
			if len(got.Reason) > bound {
				t.Errorf("%s: reason for %d argument bytes is %d bytes (bound %d); it echoes the arguments whole",
					fname, len(args), len(got.Reason), bound)
			}
		}
	}
}

// Probe 4.
func TestAdvB31SettleUnknownKindsCapAndCount(t *testing.T) {
	var d DecodingReport
	for i := 0; i < 10000; i++ {
		d.NoteUnknown(fmt.Sprintf("kind_%05d", i))
	}
	if len(d.UnknownKinds) != 16 || d.UnknownKindsMore != 10000-16 {
		t.Fatalf("len=%d more=%d, want 16 and %d", len(d.UnknownKinds), d.UnknownKindsMore, 10000-16)
	}
	for i := 1; i < len(d.UnknownKinds); i++ {
		if d.UnknownKinds[i-1] >= d.UnknownKinds[i] {
			t.Fatalf("not sorted and distinct: %v", d.UnknownKinds)
		}
	}
}

// UnknownKindsMore is documented as the count of DISTINCT types past the
// bound. One type seen many times must count once.
func TestAdvB31SettleUnknownKindsMoreCountsDistinctNotOccurrences(t *testing.T) {
	var d DecodingReport
	for i := 0; i < 16; i++ {
		d.NoteUnknown(fmt.Sprintf("kind_%02d", i))
	}
	for i := 0; i < 50; i++ {
		d.NoteUnknown("overflow_type") // one distinct type, fifty blocks
	}
	if d.UnknownKindsMore != 1 {
		t.Fatalf("UnknownKindsMore = %d for one distinct overflow type seen 50 times; documented as distinct types", d.UnknownKindsMore)
	}
}

// Names come off the wire and reach logs and a terminal.
func TestAdvB31SettleUnknownKindsAreSanitised(t *testing.T) {
	var d DecodingReport
	d.NoteUnknown("evil\x1b[2J\x1b]0;pwned\x07line\nbreak\r\x00\x7f")
	for _, k := range d.UnknownKinds {
		for _, r := range k {
			if unicode.IsControl(r) {
				t.Fatalf("control character %U survives in %q; it reaches logs and the TUI as written", r, k)
			}
		}
	}
}

func TestAdvB31SettleUnknownKindsStayValidUTF8WhenCut(t *testing.T) {
	var d DecodingReport
	d.NoteUnknown(strings.Repeat("\u20ac", 1000)) // 3-byte rune; 64 is not a multiple of 3
	d.NoteUnknown(strings.Repeat("x", 1<<20))
	for _, k := range d.UnknownKinds {
		if len(k) > 64 {
			t.Errorf("name of %d bytes kept", len(k))
		}
		if !utf8.ValidString(k) {
			t.Errorf("name cut mid-rune, invalid UTF-8: %q", k)
		}
	}
	if len(d.UnknownKinds) != 2 {
		t.Errorf("kinds = %d, want 2", len(d.UnknownKinds))
	}
}

// A real type literally called "(empty)" must not merge with the empty name.
func TestAdvB31SettleUnknownKindsEmptyNameDoesNotCollide(t *testing.T) {
	var d DecodingReport
	d.NoteUnknown("")
	d.NoteUnknown("(empty)")
	if len(d.UnknownKinds) != 2 {
		t.Fatalf("an empty type name and the literal \"(empty)\" collapsed: %v", d.UnknownKinds)
	}
}

// Two long names that differ only after the cut are two types.
func TestAdvB31SettleUnknownKindsDistinctPastTheCut(t *testing.T) {
	var d DecodingReport
	p := strings.Repeat("p", 64)
	d.NoteUnknown(p + "A")
	d.NoteUnknown(p + "B")
	if len(d.UnknownKinds)+d.UnknownKindsMore != 2 {
		t.Fatalf("two distinct types reported as %d: %v more=%d", len(d.UnknownKinds)+d.UnknownKindsMore, d.UnknownKinds, d.UnknownKindsMore)
	}
}

func TestAdvB31SettleCleanReflectsUnknownKinds(t *testing.T) {
	var d DecodingReport
	if !d.Clean() {
		t.Fatal("zero report is not clean")
	}
	d.NoteUnknown("x")
	if d.Clean() {
		t.Fatal("report with an unknown kind is clean")
	}
}
