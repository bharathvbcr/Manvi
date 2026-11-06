package agent

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

const (
	geminiCutCallBody = `event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"Let me echo that."}]}}

event: step.start
data: {"event_type":"step.start","index":1,"step":{"type":"function_call","id":"fc_1","name":"echo","arguments":{}}}

event: step.delta
data: {"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"v\":"}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"max_tokens"}}

event: done
data: [DONE]

`
	geminiAnswerBody = `event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"done"}]}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"completed"}}

event: done
data: [DONE]

`
)

// TestAGeminiCallCutByMaxTokensIsReportedToTheModelAndTheTurnContinues runs the
// real loop against the real Gemini adapter. The decoder used to return an error
// for the cut call, which ended the turn before the loop could append the prose
// or tell the model anything.
func TestAGeminiCallCutByMaxTokensIsReportedToTheModelAndTheTurnContinues(t *testing.T) {
	reqs, out, err := driveWire(t, func(url string) llm.Provider {
		return gemini.New(url, adaptertest.Secret("gemini-FIXTURE-not-a-real-key"))
	}, "gemini-3.7-flash", []string{geminiCutCallBody, geminiAnswerBody}, tools.Result{Text: "echoed"})
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	if out.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", out.Malformed)
	}
	if !out.TruncatedByTokens {
		t.Error("a max-tokens stop was not reported")
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (the turn continued)", len(reqs))
	}
	if !strings.Contains(reqs[1], "could not read") || !strings.Contains(reqs[1], "output limit") {
		t.Errorf("the model was not told its call was cut off:\n%s", reqs[1])
	}
	if !strings.Contains(reqs[1], "Let me echo that.") {
		t.Errorf("the prose streamed before the cut call was not appended to the history:\n%s", reqs[1])
	}
	if strings.Contains(reqs[1], `"id":"fc_1"`) {
		t.Errorf("the cut call was replayed as a function_call:\n%s", reqs[1])
	}
}
