package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// toolFrames is the event sequence for one tool_use block at index, optionally
// carrying one input_json_delta and optionally closed by content_block_stop.
func toolFrames(index int, id, partial string, sealed bool) string {
	out := fmt.Sprintf(`event: content_block_start
data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":%q,"name":"devcouncil_write_file"}}

`, index, id)
	if partial != "" {
		out += fmt.Sprintf(`event: content_block_delta
data: {"type":"content_block_delta","index":%d,"delta":{"type":"input_json_delta","partial_json":%s}}

`, index, quote(partial))
	}
	if sealed {
		out += fmt.Sprintf(`event: content_block_stop
data: {"type":"content_block_stop","index":%d}

`, index)
	}
	return out
}

func quote(s string) string {
	raw, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func finish(stopReason string) string {
	return `event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"` + stopReason + `"},"usage":{"output_tokens":4096}}

event: message_stop
data: {"type":"message_stop"}

`
}

func settle(t *testing.T, body string) (llm.Response, error) {
	t.Helper()
	adapter, _ := scripted(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	return resp, err
}

// TestMaxTokensMidToolUseSettlesAsAMalformedCall: the model ran out of output
// budget while writing a call's arguments. That is its own limit, not a failed
// stream, so the turn settles with StopMaxTokens and a MalformedCall the loop
// can tell the model about, exactly as openaicompat settles a cut-off call. It
// never becomes a runnable call, whether or not the cut prefix parses.
func TestMaxTokensMidToolUseSettlesAsAMalformedCall(t *testing.T) {
	cases := map[string]string{
		"sealed, arguments cut mid-string": startFrame + toolFrames(0, "toolu_1", `{"path":"a.go","content":"package ma`, true) + finish("max_tokens"),
		"never sealed, arguments cut":      startFrame + toolFrames(0, "toolu_1", `{"path":"a.go","content":"package ma`, false) + finish("max_tokens"),
		// A prefix that is itself valid JSON is still a prefix when the block
		// was never sealed: ft-074e1445's rule, that a truncated argument is
		// never repaired into a runnable call.
		"never sealed, prefix parses": startFrame + toolFrames(0, "toolu_1", `{"path":"a.go"}`, false) + finish("max_tokens"),
		"never sealed, no arguments":  startFrame + toolFrames(0, "toolu_1", ``, false) + finish("max_tokens"),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			resp, err := settle(t, body)
			if err != nil {
				t.Fatalf("a call cut off by max_tokens killed the turn: %v", err)
			}
			if resp.StopReason != llm.StopMaxTokens {
				t.Errorf("StopReason = %q, want %q", resp.StopReason, llm.StopMaxTokens)
			}
			if calls := resp.Message.ToolCalls(); len(calls) != 0 {
				t.Errorf("a cut-off call was handed to the tool layer: %+v", calls)
			}
			if len(resp.Malformed) != 1 || resp.Malformed[0].ID != "toolu_1" || resp.Malformed[0].Name != "devcouncil_write_file" ||
				resp.Malformed[0].Reason == "" {
				t.Errorf("Malformed = %+v, want one entry naming toolu_1 / devcouncil_write_file with a reason", resp.Malformed)
			}
			if resp.MaxTokensApplied != request().MaxTokens {
				t.Errorf("MaxTokensApplied = %d, want the bound the request carried (%d)", resp.MaxTokensApplied, request().MaxTokens)
			}
		})
	}
}

// TestACompleteCallBeforeATruncatedOneIsKept: only the cut-off call is
// malformed; the call that arrived whole in the same message still runs.
func TestACompleteCallBeforeATruncatedOneIsKept(t *testing.T) {
	body := startFrame +
		toolFrames(0, "toolu_1", `{"path":"a.go"}`, true) +
		toolFrames(1, "toolu_2", `{"path":"b.go","content":"x`, true) +
		finish("max_tokens")
	resp, err := settle(t, body)
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 || calls[0].ID != "toolu_1" || string(calls[0].Arguments) != `{"path":"a.go"}` {
		t.Errorf("calls = %+v, want only toolu_1", calls)
	}
	if len(resp.Malformed) != 1 || resp.Malformed[0].ID != "toolu_2" {
		t.Errorf("Malformed = %+v, want only toolu_2", resp.Malformed)
	}
}

// TestInvalidArgumentsWithoutAnOutputLimitAreStillAnError: the settlement above
// is for the model's own limit. Arguments that are not JSON on a message that
// ended for any other reason are a broken stream and keep failing.
func TestInvalidArgumentsWithoutAnOutputLimitAreStillAnError(t *testing.T) {
	for _, stop := range []string{"tool_use", "end_turn"} {
		t.Run(stop, func(t *testing.T) {
			resp, err := settle(t, startFrame+toolFrames(0, "toolu_1", `{"path":"a.go","content":"x`, true)+finish(stop))
			if err == nil {
				t.Fatalf("settled with calls=%+v malformed=%+v; invalid arguments on a %s stop must fail",
					resp.Message.ToolCalls(), resp.Malformed, stop)
			}
			if !strings.Contains(err.Error(), "incomplete arguments") {
				t.Errorf("err = %v", err)
			}
		})
	}
}

// TestAnUnsealedCallWithoutAnOutputLimitIsStillAnError keeps the existing rule
// for the case where the stream, not the model, lost the framing event.
func TestAnUnsealedCallWithoutAnOutputLimitIsStillAnError(t *testing.T) {
	if _, err := settle(t, startFrame+toolFrames(0, "toolu_1", `{"path":"a.go"}`, false)+finish("tool_use")); err == nil {
		t.Fatal("an unsealed call on a tool_use stop was dispatched")
	}
}
