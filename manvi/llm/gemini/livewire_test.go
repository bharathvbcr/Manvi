package gemini

import (
	"encoding/json"
	"testing"

	"manvi/llm"
	"manvi/llm/adaptertest"
)

// liveStream is transcribed from a recorded live interaction on 2026-08-19,
// not composed by hand.
//
// It is the shape this adapter was NOT written against, and the difference is
// total: a step.delta on this wire carries no `step` object at all, only an
// `index` naming the step and a `delta` whose own type says what it holds.
// Model text arrives exclusively as delta.text, and a tool call's arguments
// arrive exclusively as delta.arguments — the step.start that opens the call
// carries `"arguments": {}` as a placeholder.
//
// Decoding only the documented shape therefore produced turns with no answer at
// all, and tool calls dispatched with empty arguments. The published examples
// all show the documented shape, so both are decoded.
const liveStream = `event: interaction.created
data: {"interaction":{"id":"","status":"in_progress","object":"interaction","model":"gemini-3.7-flash"},"event_type":"interaction.created"}

event: interaction.status_update
data: {"interaction_id":"","status":"in_progress","event_type":"interaction.status_update"}

event: step.start
data: {"index":0,"step":{"type":"thought"},"event_type":"step.start"}

event: step.delta
data: {"index":0,"delta":{"signature":"EuYBCuMBARFNMg","type":"thought_signature"},"event_type":"step.delta"}

event: step.stop
data: {"index":0,"event_type":"step.stop"}

event: step.start
data: {"index":1,"step":{"type":"function_call","id":"call_1168334","name":"read_file","arguments":{}},"event_type":"step.start"}

event: step.delta
data: {"index":1,"delta":{"arguments":"{\"path\":","type":"arguments_delta"},"event_type":"step.delta"}

event: step.delta
data: {"index":1,"delta":{"arguments":"\"a.go\"}","type":"arguments_delta"},"event_type":"step.delta"}

event: step.stop
data: {"index":1,"event_type":"step.stop"}

event: step.start
data: {"index":2,"step":{"type":"model_output"},"event_type":"step.start"}

event: step.delta
data: {"index":2,"delta":{"text":"There are no tasks","type":"text"},"event_type":"step.delta"}

event: step.delta
data: {"index":2,"delta":{"text":" that are ready.","type":"text"},"event_type":"step.delta"}

event: step.stop
data: {"index":2,"event_type":"step.stop"}

event: interaction.completed
data: {"interaction":{"id":"","status":"completed","usage":{"total_input_tokens":95,"total_output_tokens":19,"total_thought_tokens":30}},"event_type":"interaction.completed"}

event: done
data: [DONE]

`

// TestTheLiveWireShapeDecodes is the regression this whole adapter needed.
//
// Before it, a five-scenario live benchmark ended every scenario with exit 4 —
// "the turn ended without an answer" — while the requests themselves all
// succeeded. The model was answering; the decoder was reading a field the
// server does not send.
func TestTheLiveWireShapeDecodes(t *testing.T) {
	adapter, _ := adapterFor(t, liveStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}

	if got := adaptertest.TextOf(chunks); got != "There are no tasks that are ready." {
		t.Errorf("text = %q — the model's answer was dropped", got)
	}
	if got := resp.Message.Text(); got != "There are no tasks that are ready." {
		t.Errorf("settled text = %q; the turn would be reported as having no answer", got)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if calls[0].Name != "read_file" || calls[0].ID != "call_1168334" {
		t.Errorf("call = %+v", calls[0])
	}
	// The arguments arrived only as deltas. Reading step.start alone yields
	// `{}` and dispatches the tool with nothing.
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments = %s (%v)", calls[0].Arguments, err)
	}
	if args.Path != "a.go" {
		t.Errorf("path = %q, want a.go — every tool call was being dispatched with empty arguments",
			args.Path)
	}
	if resp.Usage.ReasoningTokens != 30 {
		t.Errorf("reasoning tokens = %d, want 30", resp.Usage.ReasoningTokens)
	}
}

// TestAThoughtSignatureIsNotStoredAsReasoning.
//
// The thought step carries an opaque signature and no readable text. Recording
// it as reasoning would put base64 into the transcript and, worse, into every
// later prompt built from the log.
func TestAThoughtSignatureIsNotStoredAsReasoning(t *testing.T) {
	adapter, _ := adapterFor(t, liveStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := adaptertest.ReasoningOf(chunks); got != "" {
		t.Errorf("reasoning = %q; a signature is not thinking text", got)
	}
	for _, block := range resp.Message.Content {
		if block.Kind() == llm.KindReasoning {
			t.Errorf("a reasoning block was settled from a signature: %+v", block)
		}
	}
}

// TestTheDocumentedShapeStillDecodes guards the other direction. Every
// published example shows content blocks nested under `step`, so a stream in
// that form must keep working — decoding one shape must not mean refusing the
// other.
func TestTheDocumentedShapeStillDecodes(t *testing.T) {
	adapter, _ := adapterFor(t, happyStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := adaptertest.TextOf(chunks); got != "Reading the file." {
		t.Errorf("text = %q", got)
	}
	if got := adaptertest.ReasoningOf(chunks); got != "planning" {
		t.Errorf("reasoning = %q", got)
	}
	if len(resp.Message.ToolCalls()) != 1 {
		t.Errorf("calls = %d, want 1", len(resp.Message.ToolCalls()))
	}
}

// TestArgumentsForAnUnopenedStepAreRefused.
//
// Dropping such a delta silently is how a tool call comes to be dispatched with
// some of its arguments missing, which is worse than failing: the call runs and
// does the wrong thing.
func TestArgumentsForAnUnopenedStepAreRefused(t *testing.T) {
	const orphan = `event: step.delta
data: {"index":7,"delta":{"arguments":"{\"path\":\"a.go\"}","type":"arguments_delta"},"event_type":"step.delta"}

event: done
data: [DONE]

`
	adapter, _ := adapterFor(t, orphan)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Fatal("arguments for a step that never opened were accepted")
	}
}
