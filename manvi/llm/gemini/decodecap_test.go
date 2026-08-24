package gemini

import (
	"fmt"
	"io"
	"strings"
	"testing"
)

// TestThoughtSignaturesCountAgainstTheDecodeCap.
//
// applyStep copies the most recent thought signature into every call
// accumulator, signatures() hands them back as adapter-private replay state,
// and they are retained for the life of the stream and written to the session
// log — but decodedBytes counted only args, while the sibling anthropic decoder
// counted exactly the same field. Measured before this fix: 19.5 MiB of
// signature bytes retained, decodedBytes() reporting 0, and the 4MiB cap never
// firing. Two decoders bounding one thing must agree on what the thing is, or
// "exceeded the decode limit" means something different per provider.
func TestThoughtSignaturesCountAgainstTheDecodeCap(t *testing.T) {
	const signatureBytes = 128 << 10
	signature := strings.Repeat("A", signatureBytes)
	steps := (maxDecodedResponseBytes / signatureBytes) + 8

	var body strings.Builder
	body.WriteString("event: interaction.created\ndata: {\"event_type\":\"interaction.created\"}\n\n")
	for i := 0; i < steps; i++ {
		// A thinking step's signature, then the call it authorises. This is
		// the live wire's ordering: the signature precedes the call, which is
		// why lastSignature exists.
		fmt.Fprintf(&body,
			"event: step.delta\ndata: {\"event_type\":\"step.delta\",\"index\":%d,\"delta\":{\"type\":\"thought_signature\",\"signature\":\"%s\"}}\n\n",
			i, signature)
		fmt.Fprintf(&body,
			"event: step.start\ndata: {\"event_type\":\"step.start\",\"index\":%d,\"step\":{\"type\":\"function_call\",\"id\":\"call_%d\",\"name\":\"read_file\",\"arguments\":{}}}\n\n",
			i, i)
	}
	body.WriteString("data: [DONE]\n\n")

	s := newStream(io.NopCloser(strings.NewReader(body.String())), "m", 0)
	defer s.Close()

	var err error
	for err == nil {
		_, err = s.Next()
	}
	if err == io.EOF {
		t.Fatalf("the stream ran to completion holding %d accumulators and %d counted bytes; "+
			"maxDecodedResponseBytes (%d) never fired, because signature bytes were uncounted",
			len(s.order), s.decodedBytes(), maxDecodedResponseBytes)
	}
	if !strings.Contains(err.Error(), "decode limit") {
		t.Fatalf("stream failed with %v, want the decode-limit refusal", err)
	}
}

// TestOpenCallsCountAgainstTheDecodeCap pins the other half of the same rule:
// an accumulator that collects nothing is still retained, so opening calls
// forever cannot be free. This is the shape measured in openaicompat at 400,000
// accumulators and 98 MiB of heap with the tally reporting zero.
func TestOpenCallsCountAgainstTheDecodeCap(t *testing.T) {
	s := newStream(io.NopCloser(strings.NewReader("")), "m", 0)
	defer s.Close()

	before := s.decodedBytes()
	for i := 0; i < 32; i++ {
		s.callFor(fmt.Sprintf("call_%d", i), "read_file")
	}
	if got := s.decodedBytes(); got <= before {
		t.Errorf("thirty-two open calls holding no content counted %d bytes, was %d; "+
			"an accumulator that collects nothing is charged nothing", got, before)
	}
}

// TestAnInStreamErrorReportsItsReason.
//
// preflight.errorInFrame decodes `code` alongside `status`, and its comment
// records why: this wire uses the first, the reader was looking at the second,
// and the failures "reported an empty parenthesis where the reason belonged".
// That fix landed in the preflight scan and not in the decoder, so an error
// frame arriving mid-stream — past the point preflight stops looking — still
// reached the operator as "...Please try again later. ()".
func TestAnInStreamErrorReportsItsReason(t *testing.T) {
	for _, tc := range []struct {
		name  string
		wire  string
		want  string
		avoid string
	}{
		{
			name: "code only, which is what this wire actually sends",
			wire: `{"event_type":"error","error":{"message":"high demand","code":"invalid_request"}}`,
			want: "high demand (invalid_request)",
		},
		{
			name: "status is still preferred when present",
			wire: `{"event_type":"error","error":{"message":"high demand","status":"RESOURCE_EXHAUSTED","code":"invalid_request"}}`,
			want: "high demand (RESOURCE_EXHAUSTED)",
		},
		{
			name:  "neither: no empty parenthesis rather than a hole",
			wire:  `{"event_type":"error","error":{"message":"high demand"}}`,
			want:  "high demand",
			avoid: "()",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := "event: error\ndata: " + tc.wire + "\n\n"
			s := newStream(io.NopCloser(strings.NewReader(body)), "m", 0)
			defer s.Close()

			_, err := s.Next()
			if err == nil {
				t.Fatal("an error frame decoded without failing the stream")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("stream error = %q, want it to contain %q", err.Error(), tc.want)
			}
			if tc.avoid != "" && strings.Contains(err.Error(), tc.avoid) {
				t.Errorf("stream error = %q, must not contain %q", err.Error(), tc.avoid)
			}
		})
	}
}
