package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/session"
)

var allSessionTypes = []session.Type{
	session.TurnStart, session.TurnEnd, session.StepStart, session.StepEnd,
	session.UserMessage, session.SystemPrompt, session.AssistantChunk, session.AssistantMessage,
	session.ToolCall, session.ToolResult, session.PolicyDenied, session.PolicyQualified,
	session.GrantApplied, session.ProvenanceDropped, session.ToolResultCompacted,
	session.ContextOverflow, session.MalformedToolCall, session.DecodingCompensated,
	session.NullResponseRetried, session.VerifyReport, session.PromptAssembled,
	"unknown/type", "",
}

// FuzzProject feeds Project every session type with arbitrary payload bytes.
// It must never panic, and every event it returns must marshal and survive the
// JSON sink as a single valid NDJSON line.
func FuzzProject(f *testing.F) {
	seeds := []string{
		``, `null`, `{}`, `[]`, `"x"`, `0`, `{"usage":{"input_tokens":1},"priced":true}`,
		`{"verdict":"failed","findings":["a"],"degraded":["b"],"bounce":2}`,
		`{"digest":"ab","included":["a"],"dropped":["b"],"tokens":3}`,
		`{"usage":{"input_tokens":"nope"}}`, `{"usage":{"input_tokens":1e400}}`,
		`{"cost_nano_usd":9223372036854775808}`, `{"tool_call_id":"c","text":"t","from_bytes":1,"to_bytes":0}`,
		"{\"verdict\":\"\xff\xfe\"}", `{"extra_future_field":{"a":[1,2,3]},"usage":{}}`,
	}
	for i := range allSessionTypes {
		for _, s := range seeds {
			f.Add(uint8(i), []byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, which uint8, data []byte) {
		e := session.Event{Seq: 1, Type: allSessionTypes[int(which)%len(allSessionTypes)], Data: json.RawMessage(data)}
		outs := Project(e)
		for _, o := range outs {
			if _, err := json.Marshal(o); err != nil {
				t.Fatalf("%s: projected event does not marshal: %v (%+v)", e.Type, err, o)
			}
			var buf bytes.Buffer
			NewJSONSink(&buf, armedScrubber()).Emit(o)
			line := strings.TrimSuffix(buf.String(), "\n")
			if strings.Contains(line, "\n") || !json.Valid([]byte(line)) {
				t.Fatalf("%s: sink wrote a bad NDJSON line: %q", e.Type, line)
			}
			// The terminal faces must also survive every kind.
			var term bytes.Buffer
			NewRenderer(&term, armedScrubber()).Emit(o)
		}
	})
}

// Attack 7: a secret in every text position of every new record, including in
// JSON keys and in the child-forwarded form, must not reach the wire.
func TestAdversarialNoSecretOnTheWireFromAnyNewRecord(t *testing.T) {
	k := zfixKey
	raw := func(v any) json.RawMessage {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	events := []session.Event{
		{Type: session.VerifyReport, Data: raw(session.VerifyReportData{
			Verdict: "v-" + k, Source: "s-" + k, Paths: []string{"p-" + k},
			Findings: []string{"f-" + k}, Degraded: []string{"d-" + k},
		})},
		{Type: session.MalformedToolCall, Data: raw(session.MalformedCallData{
			ToolCallID: "id-" + "FAKE", Tool: "tool-" + k, Reason: "r-" + k})},
		{Type: session.PromptAssembled, Data: raw(session.PromptAssembledData{
			Included: []string{"i-" + k}, Dropped: []string{"x-" + k}, Digest: "d"})},
		{Type: session.ToolResultCompacted, Data: raw(session.CompactionData{
			ToolCallID: "c-FAKE", Text: "t-" + k})},
		{Type: session.ContextOverflow, Data: raw(map[string]any{"anything-" + k: "v-" + k})},
		{Type: session.DecodingCompensated, Data: raw(session.DecodingData{FallbackFormat: "ff-" + k})},
	}
	for _, e := range events {
		for _, o := range Project(e) {
			o.Agent = "child"
			var buf bytes.Buffer
			NewJSONSink(&buf, armedScrubber()).Emit(o)
			if strings.Contains(buf.String(), k) {
				t.Errorf("%s: credential on the wire: %s", e.Type, buf.String())
			}
		}
	}
}

// Attack 7: the diagnostic event must be bounded. A malformed call's name and
// id come off the wire from the model's server and are not capped by the
// adapter; the log holds them once, but the NDJSON stream now repeats them in
// every consumer's pipe (and a parent's stream repeats a child's).
func TestAdversarialDiagnosticEventIsBounded(t *testing.T) {
	huge := strings.Repeat("A", 4<<20)
	data, _ := json.Marshal(session.MalformedCallData{ToolCallID: "c", Tool: huge, Reason: "x"})
	outs := Project(session.Event{Type: session.MalformedToolCall, Data: data})
	if len(outs) != 1 {
		t.Fatalf("%d events", len(outs))
	}
	var buf bytes.Buffer
	NewJSONSink(&buf, armedScrubber()).Emit(outs[0])
	if buf.Len() > 256<<10 {
		t.Errorf("one diagnostic line is %d bytes; payloads reaching --json must be bounded", buf.Len())
	}
}

// Attack 5: forward compatibility and old logs for the projection.
func TestAdversarialStepEndPayloadShapes(t *testing.T) {
	cases := []struct {
		name string
		data string
		want int // events
	}{
		{"nil payload (old log)", ``, 0},
		{"json null", `null`, 1}, // decodes to a zero StepEndData
		{"unknown future field", `{"usage":{"input_tokens":3},"future":{"x":1},"priced":true}`, 1},
		{"truncated", `{"usage":`, 1}, // decode failure is reported, not dropped
		{"wrong type", `{"usage":"x"}`, 1},
	}
	for _, c := range cases {
		outs := Project(session.Event{Seq: 4, Type: session.StepEnd, Data: json.RawMessage(c.data)})
		if len(outs) != c.want {
			t.Errorf("%s: %d events, want %d (%+v)", c.name, len(outs), c.want, outs)
		}
	}
}
