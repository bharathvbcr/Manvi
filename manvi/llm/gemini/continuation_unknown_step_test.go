package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// TestAContinuationHoldingAnUnknownStepTypeIsRefusedByName: a step this build has
// no reading for was skipped by the comparison, so a continuation carrying one
// validated against a journal that never held it and was then replayed whole.
func TestAContinuationHoldingAnUnknownStepTypeIsRefusedByName(t *testing.T) {
	msg := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "hi"}}}
	text := json.RawMessage(`{"type":"model_output","content":[{"type":"text","text":"hi"}]}`)
	unknown := json.RawMessage(`{"type":"google_search_call","id":"s1"}`)

	if err := validateContinuation(msg, []json.RawMessage{text}); err != nil {
		t.Fatalf("a continuation of known steps was refused: %v", err)
	}
	err := validateContinuation(msg, []json.RawMessage{unknown, text})
	if err == nil || !strings.Contains(err.Error(), "google_search_call") {
		t.Fatalf("err = %v, want a refusal naming google_search_call", err)
	}
	for _, known := range []string{StepThought, StepModelOutput} {
		step := json.RawMessage(`{"type":"` + known + `"}`)
		if err := validateContinuation(msg, []json.RawMessage{step, text}); err != nil {
			t.Errorf("known step type %q refused: %v", known, err)
		}
	}
}
