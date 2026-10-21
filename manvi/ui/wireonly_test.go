package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// wireOnlyEvents is one event of each kind that exists for the NDJSON face.
func wireOnlyEvents() []Event {
	return []Event{
		{Kind: KindStepUsage, InputTokens: 10, OutputTokens: 2},
		{Kind: KindVerifyReport, Verdict: "failed", Source: "command", Findings: []string{"boom"}},
		{Kind: KindDiagnostic, Type: "context/overflow", Data: json.RawMessage(`{"threshold":1}`)},
	}
}

// The terminal says what these carry already — outcomeNotices for a bad
// verdict, turn.usage for the cost — so the renderer writes none of them. That
// includes the attribution prefix, which is written before the kind is looked
// at: a child's verify.report must not leave a bare "[label] " behind.
func TestTheRendererWritesNothingForWireOnlyKinds(t *testing.T) {
	for _, agent := range []string{"", "child-1"} {
		for _, e := range wireOnlyEvents() {
			var out bytes.Buffer
			r := NewRenderer(&out, armedScrubber())
			e.Agent = agent
			r.Emit(e)
			if out.Len() != 0 {
				t.Errorf("%s from agent %q rendered %q, want nothing", e.Kind, agent, out.String())
			}
		}
	}
}

// The wire-only list is what every terminal face consults, so it must name
// exactly the three kinds and no kind a terminal does show.
func TestWireOnlyNamesExactlyTheObservabilityKinds(t *testing.T) {
	for _, e := range wireOnlyEvents() {
		if !e.Kind.WireOnly() {
			t.Errorf("%s is not wire-only", e.Kind)
		}
	}
	for _, k := range []Kind{KindUsage, KindNotice, KindError, KindTurnEnd, KindPolicy, KindReport} {
		if k.WireOnly() {
			t.Errorf("%s is wire-only; a terminal would stop showing it", k)
		}
	}
}

// A checker's findings quote the files it read, and a file can hold a
// credential. The NDJSON face scrubs the verify report's words and the
// diagnostic payload like every other text field.
func TestTheJSONSinkScrubsAVerifyReportAndADiagnostic(t *testing.T) {
	var out bytes.Buffer
	sink := NewJSONSink(&out, armedScrubber())
	sink.Emit(Event{
		Kind: KindVerifyReport, Verdict: "failed", Source: "command",
		Paths:    []string{"config/" + zfixKey + ".env"},
		Findings: []string{"line 3: token=" + zfixKey},
		Degraded: []string{"lint " + zfixKey},
	})
	sink.Emit(Event{
		Kind: KindDiagnostic, Type: "system/prompt_assembled",
		Data: json.RawMessage(`{"dropped":["notes: ` + zfixKey + `"]}`),
	})
	if strings.Contains(out.String(), zfixKey) {
		t.Fatalf("a credential reached the NDJSON wire:\n%s", out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d lines written, want 2:\n%s", len(lines), out.String())
	}
	var got Event
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}
	if got.Kind != KindVerifyReport || got.Verdict != "failed" || len(got.Findings) != 1 {
		t.Errorf("the report did not survive scrubbing: %+v", got)
	}
}
