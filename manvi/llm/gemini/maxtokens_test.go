package gemini

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// sse renders frames as an event stream body; each pair is an event name and
// its JSON payload.
func sse(frames ...[2]string) string {
	var b strings.Builder
	for _, f := range frames {
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", f[0], f[1])
	}
	return b.String()
}

func completedWith(status string) [2]string {
	return [2]string{"interaction.completed",
		`{"event_type":"interaction.completed","interaction":{"status":"` + status + `"}}`}
}

var doneFrame = [2]string{"done", "[DONE]"}

// cutCallStream is prose, then a call whose arguments stop mid-string, then a
// terminal frame with the given status. Both wire shapes the decoder reads are
// covered by the callers: indexed deltas (the live wire, which the continuation
// retains) and the documented nested form.
func cutCallStream(status string, sealed, indexed bool) string {
	frames := [][2]string{}
	if indexed {
		frames = append(frames,
			[2]string{"step.start", `{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`},
			[2]string{"step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"Let me read it."}}`},
			[2]string{"step.stop", `{"event_type":"step.stop","index":0}`},
			[2]string{"step.start", `{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}}`},
			[2]string{"step.delta", `{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"path\":\"a."}}`},
		)
		if sealed {
			frames = append(frames, [2]string{"step.stop", `{"event_type":"step.stop","index":1}`})
		}
	} else {
		frames = append(frames,
			[2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"Let me read it."}]}}`},
			[2]string{"step.start", `{"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}`},
			[2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":"{\"path\":\"a."}}`},
		)
		if sealed {
			frames = append(frames, [2]string{"step.stop", `{"event_type":"step.stop","step":{"type":"function_call","id":"fc_1"}}`})
		}
	}
	frames = append(frames, completedWith(status), doneFrame)
	return sse(frames...)
}

func settle(t *testing.T, body string) (llm.Response, error) {
	t.Helper()
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	return resp, err
}

// assertCutCallReported is the shared expectation for a call the output limit
// cut off: the turn survives, the call is reported and not dispatched, and the
// prose that streamed before it is kept.
func assertCutCallReported(t *testing.T, resp llm.Response) {
	t.Helper()
	if resp.StopReason != llm.StopMaxTokens {
		t.Errorf("stop = %q, want %q", resp.StopReason, llm.StopMaxTokens)
	}
	if len(resp.Malformed) != 1 {
		t.Fatalf("Malformed = %+v, want exactly one", resp.Malformed)
	}
	m := resp.Malformed[0]
	if m.ID != "fc_1" || m.Name != "read_file" {
		t.Errorf("Malformed names %q/%q, want fc_1/read_file", m.ID, m.Name)
	}
	if !strings.Contains(m.Reason, "output limit") {
		t.Errorf("Reason = %q, want it to say the output limit cut the call off", m.Reason)
	}
	if got := resp.Message.Text(); got != "Let me read it." {
		t.Errorf("Message.Text() = %q, want the prose that streamed before the call", got)
	}
	if calls := resp.Message.ToolCalls(); len(calls) != 0 {
		t.Errorf("the cut call was dispatched: %+v", calls)
	}
}

// TestAMaxTokensStopReportsATruncatedCallAsMalformed.
//
// Response returned an error for a call whose arguments were not an object
// before it ever read the stop reason, so a model that ran out of output budget
// mid-call killed the turn and discarded the prose it had already produced.
// Anthropic settles the same case as a MalformedCall; this does the same.
func TestAMaxTokensStopReportsATruncatedCallAsMalformed(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sealed, indexed bool
	}{
		{"indexed and unsealed", false, true},
		{"indexed and sealed by step.stop", true, true},
		{"nested and unsealed", false, false},
		{"nested and sealed by step.stop", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := settle(t, cutCallStream("max_tokens", tc.sealed, tc.indexed))
			if err != nil {
				t.Fatalf("a call cut off by max_tokens killed the turn: %v", err)
			}
			assertCutCallReported(t, resp)
		})
	}
}

// TestAnIncompleteStatusReportsATruncatedCallAsMalformed is the same settlement
// for the other status mapStatus reads as an exhausted output budget.
func TestAnIncompleteStatusReportsATruncatedCallAsMalformed(t *testing.T) {
	for _, indexed := range []bool{true, false} {
		resp, err := settle(t, cutCallStream("incomplete", false, indexed))
		if err != nil {
			t.Fatalf("indexed=%v: a call cut off by an incomplete interaction killed the turn: %v", indexed, err)
		}
		assertCutCallReported(t, resp)
	}
}

// TestACutCallWithNoArgumentBytesAtMaxTokensIsMalformed covers the other way a
// budget ends a call: it opened, and nothing of its arguments arrived.
func TestACutCallWithNoArgumentBytesAtMaxTokensIsMalformed(t *testing.T) {
	body := sse(
		[2]string{"step.start", `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}}`},
		completedWith("max_tokens"), doneFrame)
	resp, err := settle(t, body)
	if err != nil {
		t.Fatalf("an opened call with no arguments at max_tokens killed the turn: %v", err)
	}
	if len(resp.Malformed) != 1 || resp.Malformed[0].ID != "fc_1" || len(resp.Message.ToolCalls()) != 0 {
		t.Errorf("Malformed = %+v, calls = %+v", resp.Malformed, resp.Message.ToolCalls())
	}
}

// TestAWholeCallBesideACutOneStillRunsAtMaxTokens: only the call that did not
// arrive whole is withheld; one that did is dispatched, and the continuation
// check does not trip over the withheld step.
func TestAWholeCallBesideACutOneStillRunsAtMaxTokens(t *testing.T) {
	body := sse(
		[2]string{"step.start", `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"fc_a","name":"read_file","arguments":{}}}`},
		[2]string{"step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"path\":\"a.go\"}"}}`},
		[2]string{"step.stop", `{"event_type":"step.stop","index":0}`},
		[2]string{"step.start", `{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"fc_b","name":"read_file","arguments":{}}}`},
		[2]string{"step.delta", `{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"path\":"}}`},
		completedWith("max_tokens"), doneFrame)
	resp, err := settle(t, body)
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "fc_a" {
		t.Errorf("calls = %+v, want only fc_a", calls)
	}
	if len(resp.Malformed) != 1 || resp.Malformed[0].ID != "fc_b" {
		t.Errorf("Malformed = %+v, want only fc_b", resp.Malformed)
	}
	if resp.StopReason != llm.StopMaxTokens {
		t.Errorf("stop = %q", resp.StopReason)
	}
}

// TestACutCallWithNoNameAtMaxTokensStaysAnError: a call that cannot be named
// cannot be told back to the model either, and anthropic refuses it the same way.
func TestACutCallWithNoNameAtMaxTokensStaysAnError(t *testing.T) {
	body := sse(
		[2]string{"step.start", `{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"fc_1","arguments":{}}}`},
		[2]string{"step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"p"}}`},
		completedWith("max_tokens"), doneFrame)
	if _, err := settle(t, body); err == nil || !strings.Contains(err.Error(), "no function name") {
		t.Fatalf("err = %v, want the no-function-name refusal", err)
	}
}

// TestACompletedStatusWithUnusableArgumentsIsStillAnErrorAndNeverDispatches pins
// the other half of the split: only an exhausted output budget makes unusable
// arguments the model's to be told about. Anything else is a broken stream.
func TestACompletedStatusWithUnusableArgumentsIsStillAnErrorAndNeverDispatches(t *testing.T) {
	for name, args := range map[string]string{
		"truncated JSON":                `"{\"path\":\"a."`,
		"a bare JSON string":            `"\"just a string\""`,
		"two concatenated objects":      `"{\"a\":1}{\"b\":2}"`,
		"a JSON array rather than {}":   `"[1,2]"`,
		"truncated JSON, indexed shape": ``,
	} {
		t.Run(name, func(t *testing.T) {
			var body string
			if args == "" {
				body = cutCallStream("completed", true, true)
			} else {
				body = sse(
					[2]string{"step.start", `{"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}`},
					[2]string{"step.delta", `{"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":` + args + `}}`},
					completedWith("completed"), doneFrame)
			}
			resp, err := settle(t, body)
			if err == nil {
				t.Fatalf("unusable arguments settled: %+v", resp)
			}
			if len(resp.Message.ToolCalls()) != 0 || len(resp.Malformed) != 0 {
				t.Errorf("error %v but the response carried calls %+v / malformed %+v",
					err, resp.Message.ToolCalls(), resp.Malformed)
			}
		})
	}
}

// TestAnAuthoritativeSnapshotDoesNotReplayTheWithheldCall: when the terminal
// frame carries interaction.steps, that snapshot is the replay state, and it
// lists the cut call. The message does not, so the snapshot is filtered or the
// continuation check refuses a turn the decoder had just settled.
func TestAnAuthoritativeSnapshotDoesNotReplayTheWithheldCall(t *testing.T) {
	body := sse(
		[2]string{"step.start", `{"event_type":"step.start","index":0,"step":{"type":"model_output"}}`},
		[2]string{"step.delta", `{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"Let me read it."}}`},
		[2]string{"step.start", `{"event_type":"step.start","index":1,"step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}}`},
		[2]string{"step.delta", `{"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"path\":\"a."}}`},
		[2]string{"interaction.completed", `{"event_type":"interaction.completed","interaction":{"status":"max_tokens","steps":[` +
			`{"type":"model_output","content":[{"type":"text","text":"Let me read it."}]},` +
			`{"type":"function_call","id":"fc_1","name":"read_file","arguments":{}}]}}`},
		doneFrame)
	resp, err := settle(t, body)
	if err != nil {
		t.Fatalf("turn died: %v", err)
	}
	assertCutCallReported(t, resp)
	if resp.Message.Provenance != nil && strings.Contains(string(resp.Message.Provenance.ReplayState), "fc_1") {
		t.Errorf("the withheld call is in the replay state: %s", resp.Message.Provenance.ReplayState)
	}
}
