package gemini

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
	"manvi/llm/adaptertest"
)

// reusedIDStream is two complete, well-formed tool calls that share an id,
// each framed by the step.stop the API sends when a call is finished.
//
// A server has no obligation to make ids unique across a stream and this
// decoder cannot make it. What it can do is stop reading "same id" as "same
// call" once the stream has said the first one is over — which is exactly what
// step.stop says, and step.stop was being discarded.
const reusedIDStream = `event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{"path":"a.go"}}}

event: step.stop
data: {"event_type":"step.stop","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{"path":"b.go"}}}

event: step.stop
data: {"event_type":"step.stop","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action","usage":{"total_input_tokens":10,"total_output_tokens":20,"total_thought_tokens":0}}}

event: done
data: [DONE]

`

// TestAReusedCallIDIsTwoCallsNotOneCorruptedOne.
//
// Before step.stop was honoured, callFor matched on id alone, so the second
// call's arguments were appended to the first's. Two valid objects became
// `{"path":"a.go"}{"path":"b.go"}`, json.Valid said no, and the whole turn
// failed reporting the arguments as "incomplete" — a diagnosis that sends the
// reader to look for a truncated stream that never happened. Measured against
// a fault-injecting stand-in for the live endpoint: the turn died and every
// completed step of it was discarded.
func TestAReusedCallIDIsTwoCallsNotOneCorruptedOne(t *testing.T) {
	adapter, _ := adapterFor(t, reusedIDStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("the stream failed on two well-formed calls sharing an id: %v", err)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("got %d call(s), want 2 — a reused id merged two calls into one", len(calls))
	}
	for i, want := range []string{"a.go", "b.go"} {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(calls[i].Arguments, &args); err != nil {
			t.Fatalf("call %d arguments = %s (%v)", i, calls[i].Arguments, err)
		}
		if args.Path != want {
			t.Errorf("call %d path = %q, want %q", i, args.Path, want)
		}
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop = %q, want tool_use", resp.StopReason)
	}
}

// TestStepStopDoesNotEndTheStream guards the other direction: honouring
// step.stop must not make it terminal. A turn with a call and then prose has
// text after the stop frame, and swallowing the rest would silently shorten
// every such answer.
func TestStepStopDoesNotEndTheStream(t *testing.T) {
	const body = `event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":{"path":"a.go"}}}

event: step.stop
data: {"event_type":"step.stop","step":{"type":"function_call","id":"fc_1"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"model_output","content":[{"type":"text","text":"and then some prose"}]}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action"}}

event: done
data: [DONE]

`
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if got := adaptertest.TextOf(chunks); got != "and then some prose" {
		t.Errorf("text after step.stop = %q — the stop frame swallowed the rest of the stream", got)
	}
	if len(resp.Message.ToolCalls()) != 1 {
		t.Errorf("calls = %d, want 1", len(resp.Message.ToolCalls()))
	}
}

// TestUnusableArgumentsAreNotCalledIncomplete pins the diagnosis, because the
// wrong one costs an investigation. Arguments that are two objects run
// together are complete and unusable; telling a reader the stream was cut off
// sends them to the wrong half of the system.
func TestUnusableArgumentsAreNotCalledIncomplete(t *testing.T) {
	const truncated = `event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","name":"read_file","arguments":"{\"path\":\"a."}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"completed"}}

event: done
data: [DONE]

`
	adapter, _ := adapterFor(t, truncated)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err == nil {
		t.Fatal("unusable arguments were accepted; a call the decoder cannot read must never be dispatched")
	} else {
		if !strings.Contains(err.Error(), "not a JSON object") {
			t.Errorf("error = %q, want it to say the arguments are not a JSON object", err)
		}
		if !strings.Contains(err.Error(), "not dispatched") {
			t.Errorf("error = %q, want it to say the call was not dispatched", err)
		}
	}
}

// splitArgumentStream carries one call's arguments as successive JSON string
// fragments, which is the only shape a streaming API can use for a blob too
// long to fit one frame -- an SSE data line must be valid JSON on its own, so
// half an object cannot be sent as raw JSON at all.
//
// This is the shape a wide fan-out produces: devcouncil_spawn_subagents with
// eight labelled prompts is a large argument object, and it is exactly the call
// a multi-agent benchmark makes first.
const splitArgumentStream = `event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_9","name":"spawn"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_9","arguments":"{\"tasks\":[{\"lab"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_9","arguments":"el\":\"a\",\"prompt\":\"do"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_9","arguments":" a thing\"}]}"}}

event: step.stop
data: {"event_type":"step.stop","step":{"type":"function_call","id":"fc_9"}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action"}}

event: done
data: [DONE]

`

// TestArgumentsSplitAcrossStringFragmentsReassemble.
//
// Before the fragments were unquoted, this stream produced
// `"{\"tasks\":[{\"lab""el\":..."` -- three quoted strings run together,
// not valid JSON -- and the turn died reporting incomplete arguments. A single
// fragment was worse: it passed the old validity check, because a JSON string
// is valid JSON, and a call was dispatched whose arguments were a quoted string
// instead of an object.
func TestArgumentsSplitAcrossStringFragmentsReassemble(t *testing.T) {
	adapter, _ := adapterFor(t, splitArgumentStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("a call whose arguments were streamed as string fragments failed: %v", err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	var args struct {
		Tasks []struct {
			Label  string `json:"label"`
			Prompt string `json:"prompt"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("reassembled arguments = %s (%v)", calls[0].Arguments, err)
	}
	if len(args.Tasks) != 1 || args.Tasks[0].Label != "a" || args.Tasks[0].Prompt != "do a thing" {
		t.Fatalf("reassembled arguments = %+v", args)
	}
}

// TestASingleStringFragmentIsUnwrappedNotDispatchedAsAString is the quiet half
// of the same defect: one whole object sent as a JSON string used to pass
// validation and reach the tool as a quoted string.
func TestASingleStringFragmentIsUnwrappedNotDispatchedAsAString(t *testing.T) {
	const body = `event: step.start
data: {"event_type":"step.start","step":{"type":"function_call","id":"fc_1","name":"read_file"}}

event: step.delta
data: {"event_type":"step.delta","step":{"type":"function_call","id":"fc_1","arguments":"{\"path\":\"a.go\"}"}}

event: interaction.completed
data: {"event_type":"interaction.completed","interaction":{"status":"requires_action"}}

event: done
data: [DONE]

`
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	calls := resp.Message.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(calls[0].Arguments, &args); err != nil {
		t.Fatalf("arguments = %s (%v) — a string-form payload reached the tool unwrapped",
			calls[0].Arguments, err)
	}
	if args.Path != "a.go" {
		t.Errorf("path = %q, want a.go", args.Path)
	}
}
