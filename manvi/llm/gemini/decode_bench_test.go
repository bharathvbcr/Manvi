package gemini

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// benchStream is the shape a real turn has: a thought step and its signature,
// a call whose arguments arrive as fragments, and an answer that arrives as
// hundreds of small text deltas. The deltas are the point. They are nearly
// every frame of a response, and each one is decoded on the path between the
// socket and the caller.
func benchStream() []byte {
	var b strings.Builder
	frame := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	frame("interaction.created", `{"interaction":{"id":"","status":"in_progress"},"event_type":"interaction.created"}`)
	frame("step.start", `{"index":0,"step":{"type":"thought"},"event_type":"step.start"}`)
	frame("step.delta", `{"index":0,"delta":{"signature":"EuYBCuMBARFNMgEuYBCuMBARFNMg","type":"thought_signature"},"event_type":"step.delta"}`)
	frame("step.stop", `{"index":0,"event_type":"step.stop"}`)
	frame("step.start", `{"index":1,"step":{"type":"function_call","id":"call_1","name":"read_file","arguments":{}},"event_type":"step.start"}`)
	frame("step.delta", `{"index":1,"delta":{"arguments":"{\"path\":\"","type":"arguments_delta"},"event_type":"step.delta"}`)
	for i := 0; i < 20; i++ {
		frame("step.delta", fmt.Sprintf(`{"index":1,"delta":{"arguments":"part%02d/","type":"arguments_delta"},"event_type":"step.delta"}`, i))
	}
	frame("step.delta", `{"index":1,"delta":{"arguments":"a.go\"}","type":"arguments_delta"},"event_type":"step.delta"}`)
	frame("step.stop", `{"index":1,"event_type":"step.stop"}`)
	frame("step.start", `{"index":2,"step":{"type":"model_output"},"event_type":"step.start"}`)
	for i := 0; i < 400; i++ {
		frame("step.delta", fmt.Sprintf(`{"index":2,"delta":{"text":"token %03d of the answer ","type":"text"},"event_type":"step.delta"}`, i))
	}
	frame("step.stop", `{"index":2,"event_type":"step.stop"}`)
	frame("interaction.completed", `{"interaction":{"id":"","status":"completed","usage":{"total_input_tokens":95,"total_output_tokens":19,"total_thought_tokens":30}},"event_type":"interaction.completed"}`)
	frame("done", `[DONE]`)
	return []byte(b.String())
}

// BenchmarkStreamDecode compares the single-pass frame decode with the
// two-pass path it replaced, which frames the single pass refuses still take.
func BenchmarkStreamDecode(b *testing.B) {
	body := benchStream()
	for _, tc := range []struct {
		name  string
		twice bool
	}{{"two-decodes", true}, {"one-decode", false}} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(body)))
			b.ReportAllocs()
			for b.Loop() {
				s := newStream(io.NopCloser(bytes.NewReader(body)), "gemini-3.7-flash", 0)
				s.decodeTwice = tc.twice
				for {
					if _, err := s.Next(); err != nil {
						if !errors.Is(err, io.EOF) {
							b.Fatal(err)
						}
						break
					}
				}
				if _, err := s.Response(); err != nil {
					b.Fatal(err)
				}
				_ = s.Close()
			}
		})
	}
}
