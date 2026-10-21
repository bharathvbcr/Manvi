package tui

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// The transcript has no line for the NDJSON-only kinds. Left to the default
// path each would become an entry with nothing to draw — a blank row in the
// scrollback for every step of every turn — and a step.usage must not be added
// to the status bar's totals either, because turn.usage already is.
func TestTheTranscriptDropsWireOnlyKinds(t *testing.T) {
	events := []ui.Event{
		{Kind: ui.KindStepUsage, InputTokens: 10, OutputTokens: 2},
		{Kind: ui.KindVerifyReport, Verdict: "failed", Findings: []string{"boom"}},
		{Kind: ui.KindDiagnostic, Type: "context/overflow", Data: json.RawMessage(`{}`)},
	}

	s := NewScrollback()
	for _, e := range events {
		s.Append(e)
	}
	if s.Len() != 0 {
		t.Errorf("the scrollback holds %d entries for wire-only events, want none", s.Len())
	}

	v := NewAgentView("t", "s")
	for _, e := range events {
		v.Apply(e)
	}
	if v.Scroll.Len() != 0 {
		t.Errorf("the agent view holds %d entries for wire-only events, want none", v.Scroll.Len())
	}
	if v.Status.InputTokens != 0 || v.Status.OutputTokens != 0 {
		t.Errorf("status totals = %d in / %d out; step.usage must not be summed beside turn.usage",
			v.Status.InputTokens, v.Status.OutputTokens)
	}

	// A kind the transcript does show still gets its entry.
	s.Append(ui.Event{Kind: ui.KindNotice, Text: "hello"})
	if s.Len() != 1 {
		t.Errorf("a notice produced %d entries, want 1", s.Len())
	}
}
