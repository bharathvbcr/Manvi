package anthropic

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"manvi/llm"
)

// FuzzStreamNeverSettlesIntoAToolCallItCannotRoute fuzzes the streaming decoder
// against arbitrary bytes on the wire.
//
// This is the least controlled input the harness reads that is not a file: an
// SSE stream from a hosted endpoint, reassembled here into the assistant
// message the turn then acts on. What comes out of it is not text alone — a
// tool call decoded here is a tool the harness runs, so the decoder is the last
// thing standing between a malformed frame and an execution.
//
// The invariants are all one shape: what settles must be something a caller can
// act on, or an error. Never a third answer that reads like a clean response.
//
//   - Draining never panics and always terminates, whatever the bytes.
//   - A response returned without an error carries a stop reason. The empty
//     string is what `llm.StopReason` zero-values to, and a caller switching on
//     it would fall through every arm — which is how a turn ends with nobody
//     able to say whether it finished, the shape verify.sh records as a Gemini
//     arm with 315 episodes and zero `finished` stops.
//   - Every tool call it settles is routable: a name to dispatch on and
//     arguments that are whole JSON. A nameless call reaches the tool layer as
//     a lookup for "", and arguments that arrived truncated are the case the
//     decoder's own json.Valid guard exists for — asserted here rather than
//     trusted, because a guard is a claim until something tries to pass it.
//   - Content and tool calls only exist for blocks the stream actually opened,
//     so a settled message cannot carry more blocks than the stream announced.
func FuzzStreamNeverSettlesIntoAToolCallItCannotRoute(f *testing.F) {
	event := func(name, data string) string {
		return "event: " + name + "\ndata: " + data + "\n\n"
	}
	full := event("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":3}}}`) +
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`) +
		event("message_stop", `{"type":"message_stop"}`)

	toolCall := event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"tu_1","name":"read_file"}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":"}}`) +
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"a.go\"}"}}`) +
		event("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`) +
		event("message_stop", `{"type":"message_stop"}`)

	for _, seed := range []string{
		full,
		toolCall,
		// A tool block that never names itself.
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use"}}`) +
			event("message_stop", `{"type":"message_stop"}`),
		// Arguments that stop mid-object.
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"n"}}`) +
			event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`) +
			event("message_stop", `{"type":"message_stop"}`),
		// Stops with no stop_reason at all.
		event("message_stop", `{"type":"message_stop"}`),
		// The name and the payload type disagreeing.
		event("message_stop", `{"type":"content_block_stop"}`),
		event("error", `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`),
		event("content_block_start", `{"type":"content_block_start","index":9223372036854775807,"content_block":{"type":"text"}}`),
		event("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"orphan"}}`),
		"data: not json\n\n",
		"event: message_stop\n\n",
		"",
		"\n\n\n",
		"data: null\n\n",
		"data: {}\n\n",
		"garbage that is not sse at all",
		"data: " + strings.Repeat("x", 4096) + "\n\n",
		full + full,
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, body string) {
		s := newStream(io.NopCloser(strings.NewReader(body)), "fuzz-model", 0)
		defer func() {
			if err := s.Close(); err != nil {
				t.Errorf("closing the stream: %v", err)
			}
		}()

		// Bounded rather than `for {}`. A decoder that stopped advancing would
		// otherwise hang the fuzzer instead of failing it, and "it never
		// terminates" is one of the answers being tested for.
		const maxEvents = 100000
		drained := false
		for i := 0; i < maxEvents; i++ {
			if _, err := s.Next(); err != nil {
				drained = true
				break
			}
		}
		if !drained {
			t.Fatalf("the stream produced %d chunks without ending; a decoder that never "+
				"stops advancing holds the turn open forever", maxEvents)
		}

		resp, err := s.Response()
		if err != nil {
			return // a refusal is an answer; the invariants below are about the other one
		}

		if resp.StopReason == "" {
			t.Fatalf("a response settled with an empty stop reason; a caller switching on it " +
				"falls through every arm and the turn ends with nobody able to say how")
		}

		blocks := 0
		for _, block := range resp.Message.Content {
			blocks++
			call, ok := block.(llm.ToolCallBlock)
			if !ok {
				continue
			}
			if strings.TrimSpace(call.Name) == "" {
				t.Fatalf("a tool call settled with no name (id %q, args %q); the tool layer "+
					"would look up the empty string and the caller cannot tell that from a "+
					"tool that does not exist", call.ID, call.Arguments)
			}
			if len(call.Arguments) == 0 {
				t.Fatalf("tool call %q settled with no arguments at all; the decoder's own "+
					"contract is that an absent argument object becomes {}", call.Name)
			}
			if !json.Valid(call.Arguments) {
				t.Fatalf("tool call %q settled with arguments that are not whole JSON: %q",
					call.Name, call.Arguments)
			}
		}

		// A settled message cannot carry more blocks than the stream opened.
		// Counted from the bytes rather than from the decoder's own bookkeeping,
		// so a decoder that invented a block cannot also invent the evidence
		// that it was announced.
		if opened := strings.Count(body, `"content_block_start"`); blocks > opened {
			t.Fatalf("the message settled with %d content block(s) from a stream that opened %d; "+
				"the extra ones came from nowhere", blocks, opened)
		}
	})
}
