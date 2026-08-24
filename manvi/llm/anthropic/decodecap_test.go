package anthropic

import (
	"fmt"
	"io"
	"strings"
	"testing"

	"manvi/llm/transport"
)

// TestOpenBlocksCountAgainstTheDecodeCap.
//
// decodedBytes summed what each accumulator had *collected* and charged nothing
// for the accumulator itself, so a server that opened content blocks and put no
// content in them allocated one struct and one map entry per event while the
// tally stayed at zero and maxDecodedResponseBytes could never fire. It is the
// same shape as the openaicompat bypass measured at 400,000 accumulators and
// 98 MiB of heap: a cap that counts only the bytes the server chose to send is
// a cap the server chooses whether to be bound by.
func TestOpenBlocksCountAgainstTheDecodeCap(t *testing.T) {
	blocks := (maxDecodedResponseBytes / transport.RetainedAccumulatorBytes) + 1024

	var body strings.Builder
	for i := 0; i < blocks; i++ {
		fmt.Fprintf(&body,
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"text\"}}\n\n",
			i)
	}

	s := newStream(io.NopCloser(strings.NewReader(body.String())), "m", 0)
	defer s.Close()

	var err error
	for err == nil {
		_, err = s.Next()
	}
	if err == io.EOF {
		t.Fatalf("the stream ran to completion holding %d blocks and %d counted bytes; "+
			"maxDecodedResponseBytes (%d) never fired, because an empty accumulator was free",
			len(s.blocks), s.decodedBytes(), maxDecodedResponseBytes)
	}
	if !strings.Contains(err.Error(), "decode limit") {
		t.Fatalf("stream failed with %v, want the decode-limit refusal", err)
	}
}

// TestAnOrdinaryResponseIsNotChargedIntoRefusal is the control: a handful of
// blocks carrying real content must stay far under the ceiling, so the charge
// per accumulator cannot refuse a legitimate answer, and that the running
// tally is actually tracking rather than sitting at zero.
func TestAnOrdinaryResponseIsNotChargedIntoRefusal(t *testing.T) {
	var body strings.Builder
	for i := 0; i < 16; i++ {
		fmt.Fprintf(&body,
			"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":%d,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_%d\",\"name\":\"read_file\"}}\n\n",
			i, i)
		fmt.Fprintf(&body,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":%d,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"path\\\":\\\"/etc/hosts\\\"}\"}}\n\n",
			i)
	}

	s := newStream(io.NopCloser(strings.NewReader(body.String())), "m", 0)
	defer s.Close()

	var err error
	for err == nil {
		_, err = s.Next()
	}
	if err != io.EOF {
		t.Fatalf("sixteen ordinary tool calls failed the stream: %v", err)
	}
	got := s.decodedBytes()
	if got > maxDecodedResponseBytes {
		t.Errorf("sixteen ordinary blocks counted %d bytes, past the %d ceiling",
			got, maxDecodedResponseBytes)
	}
	if got == 0 {
		t.Errorf("decodedBytes() = 0 after sixteen blocks with arguments; the tally is not tracking")
	}
}
