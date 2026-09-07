package gemini

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// FuzzStreamNeverSettlesIntoAToolCallItCannotRoute fuzzes the streaming decoder
// against arbitrary bytes on the wire.
//
// The twin of the target of the same name in the anthropic adapter, and here
// for the same reason: this is a hosted endpoint's SSE stream reassembled into
// the assistant message a turn then acts on, and a tool call decoded out of it
// is a tool the harness runs. Three ports of one contract live in this
// repository, and the one defect the anthropic target found was a rule two of
// the three enforced and the third did not — which is an argument for holding
// all three to the same stated invariants rather than to their own tests.
//
// This adapter carries a second history worth pinning. verify.sh records a
// Gemini serialization defect that produced a 315-episode benchmark arm with
// zero `finished` stops before anything noticed: a stream that settles with no
// usable stop reason does not fail, it quietly ends every turn as "other".
//
// The invariants:
//
//   - Draining never panics and always terminates, whatever the bytes.
//   - A response returned without an error carries a stop reason.
//   - Every tool call it settles is routable: a non-empty function name and
//     arguments that are whole JSON.
//   - It never settles more function calls than the stream carried parts for.
func FuzzStreamNeverSettlesIntoAToolCallItCannotRoute(f *testing.F) {
	data := func(payload string) string { return "data: " + payload + "\n\n" }

	text := data(`{"candidates":[{"content":{"parts":[{"text":"hello"}]},"finishReason":"STOP"}]}`)
	call := data(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read_file","args":{"path":"a.go"}}}]},"finishReason":"STOP"}]}`)

	for _, seed := range []string{
		text,
		call,
		text + data(DoneSentinel),
		// A function call that never names itself — the shape the anthropic
		// twin found unguarded there and which this adapter already refuses.
		data(`{"candidates":[{"content":{"parts":[{"functionCall":{"args":{"a":1}}}]}}]}`),
		data(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"","args":{}}}]}}]}`),
		data(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"n"}}]}}]}`),
		// No finishReason at all.
		data(`{"candidates":[{"content":{"parts":[{"text":"unfinished"}]}}]}`),
		data(`{"candidates":[{"finishReason":"SAFETY"}]}`),
		data(`{"candidates":[{"finishReason":"MAX_TOKENS"}]}`),
		data(`{"candidates":[]}`),
		data(`{"candidates":null}`),
		data(`{"promptFeedback":{"blockReason":"SAFETY"}}`),
		data(`{"error":{"code":429,"message":"rate limited"}}`),
		data(`{}`),
		data(`null`),
		data(`[]`),
		data(`not json`),
		"",
		"\n\n\n",
		"garbage that is not sse at all",
		"data: " + strings.Repeat("x", 4096) + "\n\n",
		text + text,
		call + call,
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

		// Bounded rather than `for {}`: a decoder that stopped advancing would
		// hang the fuzzer instead of failing it, and never terminating is one
		// of the answers being tested for.
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
			return
		}

		if resp.StopReason == "" {
			t.Fatalf("a response settled with an empty stop reason; a caller switching on it " +
				"falls through every arm and the turn ends with nobody able to say how")
		}

		calls := 0
		for _, block := range resp.Message.Content {
			call, ok := block.(llm.ToolCallBlock)
			if !ok {
				continue
			}
			calls++
			if strings.TrimSpace(call.Name) == "" {
				t.Fatalf("a tool call settled with no name (id %q, args %q); the tool layer "+
					"would look up the empty string and cannot tell that from a tool that "+
					"does not exist", call.ID, call.Arguments)
			}
			if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
				t.Fatalf("tool call %q settled with arguments that are not whole JSON: %q",
					call.Name, call.Arguments)
			}
		}

		// Counted from the bytes rather than from the decoder's bookkeeping, so
		// a decoder that invented a call cannot also invent the evidence that
		// the stream carried it.
		if carried := strings.Count(body, "functionCall"); calls > carried {
			t.Fatalf("the message settled with %d tool call(s) from a stream carrying %d "+
				"functionCall part(s); the extra ones came from nowhere", calls, carried)
		}
	})
}
