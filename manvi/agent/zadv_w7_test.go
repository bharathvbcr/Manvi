package agent

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

const geminiWholePlusCutBody = `event: step.delta
data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"SIG-A"}}

event: step.start
data: {"event_type":"step.start","index":1,"step":{"type":"function_call","id":"fc_a","name":"echo","arguments":{}}}

event: step.delta
data: {"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"v\":1}"}}

event: step.stop
data: {"event_type":"step.stop","index":1}

event: step.start
data: {"event_type":"step.start","index":2,"step":{"type":"function_call","id":"fc_b","name":"echo","arguments":{}}}

event: step.delta
data: {"event_type":"step.delta","index":2,"delta":{"type":"arguments_delta","arguments":"{\"v\":"}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"max_tokens"}}

event: done
data: [DONE]

`

// A whole call beside a cut one at max_tokens: the loop must run the whole call,
// tell the model the other was cut off, and send the next request without an
// orphan call or result.
func TestZAdvGeminiWholePlusCutCallAtMaxTokens(t *testing.T) {
	calls := 0
	reqs, out, err := driveWire(t, func(url string) llm.Provider {
		return gemini.New(url, adaptertest.Secret("gemini-FIXTURE-not-a-real-key"))
	}, "gemini-3.7-flash", []string{geminiWholePlusCutBody, geminiAnswerBody}, tools.Result{Text: "echoed"})
	_ = calls
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	t.Logf("outcome=%+v\nreq2=%s", out, reqs[1])
	if out.Malformed != 1 {
		t.Errorf("Malformed = %d, want 1", out.Malformed)
	}
	r := reqs[1]
	if !strings.Contains(r, `"fc_a"`) {
		t.Errorf("the whole call fc_a is missing from the next request:\n%s", r)
	}
	if strings.Contains(r, `"id":"fc_b"`) {
		t.Errorf("the cut call fc_b was replayed:\n%s", r)
	}
	if !strings.Contains(r, "echoed") {
		t.Errorf("the whole call's result was not sent back:\n%s", r)
	}
	if !strings.Contains(r, "output limit") {
		t.Errorf("the cut call was not reported to the model:\n%s", r)
	}
	// every function_result must have its function_call in the same request
	nCalls := strings.Count(r, `"type":"function_call"`)
	nResults := strings.Count(r, `"type":"function_result"`)
	if nResults > nCalls {
		t.Errorf("%d function_result(s) for %d function_call(s): an orphan result", nResults, nCalls)
	}
}
