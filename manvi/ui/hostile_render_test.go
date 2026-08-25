package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"manvi/credentials"
)

// render.go states that every path writing content goes through `safe`, which
// scrubs credentials and neutralises control sequences. That is a claim about
// *every* string field of *every* event kind, and a claim of that shape is only
// worth what checks it — a field added later, or one rendered with a bare `%s`,
// is a hole nobody notices because the common case still looks right.
//
// Model output reaches this renderer. A terminal reading a raw escape from it
// will move the cursor, repaint lines the operator has already read, retitle
// the window, or — with OSC 52 — put text on the system clipboard. The evidence
// stream is the one surface an operator trusts to say what happened, so text
// that can rewrite it is the whole problem.

const fakeCredential = "sk-ant-api03-NOTAREALKEYAAAAAAAAAAAAAAAAAAAA"

// hostilePayloads are real terminal capabilities, not synthetic bytes.
var hostilePayloads = map[string]string{
	"CSI colour":         "\x1b[31mRED",
	"cursor up and wipe": "\x1b[5A\x1b[2Koverwritten",
	"clear screen":       "\x1b[2J\x1b[H",
	"OSC 0 window title": "\x1b]0;pwned\x07",
	"OSC 52 clipboard":   "\x1b]52;c;cHduZWQ=\x07",
	"bare CR overwrite":  "harmless\rMALICIOUS",
	"backspace erase":    "denied\b\b\b\b\b\ballowed",
	"bidi override":      "‮gnp.txt‭",
	"NUL":                "before\x00after",
	"DEL":                "before\x7fafter",
	"alternate screen":   "\x1b[?1049h",
	"scroll region":      "\x1b[1;5r",
}

// eventsCarrying builds one event of every kind with the payload in every
// string field it has, so no kind's rendering path is left unexercised.
func eventsCarrying(payload string) []Event {
	kinds := []Kind{
		KindSessionStart, KindTurnStart, KindText, KindReasoning,
		KindToolStart, KindToolResult, KindApproval, KindApprovalDone,
		KindPolicy, KindLease, KindUsage, KindTurnEnd, KindReport,
		KindError, KindNotice,
	}
	quoted, err := json.Marshal(payload)
	if err != nil {
		quoted = []byte(`""`)
	}
	events := make([]Event, 0, len(kinds))
	for _, kind := range kinds {
		events = append(events, Event{
			Kind: kind, At: time.Unix(0, 0).UTC(),
			Agent: payload, Text: payload, Detail: payload,
			Tool: payload, Arguments: json.RawMessage(`{"p":` + string(quoted) + `}`),
			Rule: payload, Severity: payload, Path: payload,
			GrantID: payload, GrantedBy: payload, Demoted: payload,
			Degraded: []string{payload}, Weakened: []string{payload},
			ApprovalID: payload, TaskID: payload, Posture: payload, Model: payload,
		})
	}
	return events
}

// The renderer applies styling of its own, so the assertion cannot be "no ESC
// at all". It is narrower and it is the one that matters: nothing the *payload*
// introduced may survive. The palette is emptied so anything left is content.
func TestNoHostileSequenceFromAnEventFieldReachesTheTerminal(t *testing.T) {
	forbidden := []struct {
		name string
		seq  string
	}{
		{"ESC", "\x1b"},
		{"NUL", "\x00"},
		{"DEL", "\x7f"},
		{"BEL", "\x07"},
		{"backspace", "\b"},
		{"carriage return", "\r"},
		{"bidi RLO", "‮"},
		{"bidi LRO", "‭"},
	}
	for name, payload := range hostilePayloads {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			r := NewRenderer(&out, credentials.NewScrubber())
			r.SetPalette(Palette{})
			for _, e := range eventsCarrying(payload) {
				r.Emit(e)
			}
			got := out.String()
			for _, bad := range forbidden {
				if strings.Contains(got, bad.seq) {
					t.Errorf("a %s from the payload reached the terminal.\npayload: %q\noutput:  %q",
						bad.name, payload, got)
				}
			}
		})
	}
}

// The credential backstop rides the same seam, so it is checked on every field
// rather than only on the one everybody remembers.
func TestACredentialInAnyEventFieldIsScrubbedFromTheTerminal(t *testing.T) {
	scrubber := credentials.NewScrubber()
	scrubber.Watch(credentials.NewSecret(fakeCredential, "TEST_API_KEY"))

	var out bytes.Buffer
	r := NewRenderer(&out, scrubber)
	r.SetPalette(Palette{})
	for _, e := range eventsCarrying("key=" + fakeCredential) {
		r.Emit(e)
	}
	if strings.Contains(out.String(), fakeCredential) {
		t.Fatalf("a watched credential reached the terminal:\n%s", out.String())
	}
}

// The JSON face deliberately keeps control bytes — its own comment says a
// consumer of that stream is a program and escaping the text would corrupt the
// record. What it does promise is that every line is valid JSON, and that a
// credential in it is removed: "a credential in it is a credential on disk".
//
// So this asserts the promise the sink makes rather than the terminal's rule,
// and it asserts it across every field.
func TestTheJSONFaceStaysParseableAndCarriesNoCredential(t *testing.T) {
	for name, payload := range hostilePayloads {
		t.Run(name, func(t *testing.T) {
			scrubber := credentials.NewScrubber()
			scrubber.Watch(credentials.NewSecret(fakeCredential, "TEST_API_KEY"))

			var out bytes.Buffer
			sink := NewJSONSink(&out, scrubber)
			for _, e := range eventsCarrying(payload + fakeCredential) {
				sink.Emit(e)
			}
			for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
				if line == "" {
					continue
				}
				var decoded map[string]any
				if err := json.Unmarshal([]byte(line), &decoded); err != nil {
					t.Fatalf("the JSON face emitted a line that is not JSON: %v\nline: %q", err, line)
				}
			}
			if strings.Contains(out.String(), fakeCredential) {
				t.Errorf("a watched credential reached the JSON face, which writes to disk")
			}
		})
	}
}
