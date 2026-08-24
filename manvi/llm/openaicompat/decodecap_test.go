package openaicompat

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"manvi/llm"
	"manvi/llm/transport"
)

// TestToolCallBookkeepingCountsAgainstTheDecodeCap.
//
// MaxDecodedResponseBytes is documented as counting "text, reasoning and all
// tool-call arguments together, because a per-field cap is evaded by whichever
// field the runaway happens to be filling". The runaway found a field it did
// not count: a stream of tool_calls fragments that each carry a fresh id and
// *zero* argument bytes allocates a callAccumulator, three map entries and a
// callOrder slot per fragment, and every one of those is retained for the life
// of the stream. Measured before this fix: 400,000 accumulators, 98 MiB of
// heap, decodedBytes() reporting 0, and the cap never firing.
//
// This drives the real decoder over the real wire shape, so what is pinned is
// the refusal reaching the caller, not just the arithmetic.
func TestToolCallBookkeepingCountsAgainstTheDecodeCap(t *testing.T) {
	// One accumulator is charged transport.RetainedAccumulatorBytes, so this
	// is comfortably past MaxDecodedResponseBytes while sending under two
	// megabytes of wire and not one byte of content.
	fragments := (MaxDecodedResponseBytes / transport.RetainedAccumulatorBytes) + 2048

	var body strings.Builder
	for i := 0; i < fragments; i++ {
		fmt.Fprintf(&body,
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":%d,\"id\":\"call_%d\",\"function\":{\"arguments\":\"\"}}]}}]}\n\n",
			i, i)
	}
	body.WriteString("data: [DONE]\n\n")

	s := newStream(io.NopCloser(strings.NewReader(body.String())), "m", nil,
		Options{Name: "test", Validate: func(llm.Request) error { return nil }}, 0)
	defer s.Close()

	var err error
	for err == nil {
		_, err = s.Next()
	}
	if err == io.EOF {
		t.Fatalf("the stream ran to completion holding %d accumulators and %d counted bytes; "+
			"MaxDecodedResponseBytes (%d) never fired, because per-call bookkeeping was uncounted",
			len(s.calls), s.decodedBytes(), MaxDecodedResponseBytes)
	}
	if !strings.Contains(err.Error(), "decode limit") {
		t.Fatalf("stream failed with %v, want the decode-limit refusal", err)
	}
	if s.decodedBytes() <= MaxDecodedResponseBytes {
		t.Errorf("decodedBytes() = %d, want more than %d", s.decodedBytes(), MaxDecodedResponseBytes)
	}
}

// TestAnOrdinaryToolCallIsNotChargedIntoRefusal is the control: the charge per
// accumulator must be small enough that a real response — a handful of calls
// carrying real arguments — is nowhere near the ceiling.
func TestAnOrdinaryToolCallIsNotChargedIntoRefusal(t *testing.T) {
	s := newStream(io.NopCloser(strings.NewReader("data: [DONE]\n\n")), "m", nil,
		Options{Name: "test", Validate: func(llm.Request) error { return nil }}, 0)
	defer s.Close()

	for i := 0; i < 8; i++ {
		s.applyToolCalls([]wireToolCall{{
			Index: i,
			ID:    fmt.Sprintf("call_%d", i),
			Function: wireCallFunc{
				Name:      "read_file",
				Arguments: `{"path":"/etc/hosts"}`,
			},
		}})
	}
	if got := s.decodedBytes(); got > MaxDecodedResponseBytes {
		t.Errorf("eight ordinary tool calls counted %d bytes, past the %d ceiling",
			got, MaxDecodedResponseBytes)
	}
}

// TestTruncateForMessageDoesNotSplitARune.
//
// The truncated fragment goes into MalformedCall.Reason, which is quoted back
// to the model and written to the session log. Slicing at a byte offset cut
// multi-byte UTF-8 in half and put the orphaned continuation bytes into both —
// observed as "aaa…\xe6\x97…". Nothing rejected it, because json.Marshal
// substitutes U+FFFD rather than failing, so the record was corrupted quietly.
func TestTruncateForMessageDoesNotSplitARune(t *testing.T) {
	// Offsets chosen so the 120-byte cut lands one and two bytes into a
	// three-byte rune, and exactly on a boundary.
	for _, lead := range []int{118, 119, 120} {
		input := strings.Repeat("a", lead) + strings.Repeat("日", 8)
		got := truncateForMessage(input)
		if !utf8.ValidString(got) {
			t.Errorf("truncateForMessage(%d ASCII + kanji) = %q (% x): not valid UTF-8",
				lead, got, got)
		}
		if !strings.HasSuffix(got, "…") {
			t.Errorf("truncateForMessage(%d ASCII + kanji) = %q: lost the ellipsis", lead, got)
		}
		if !strings.HasPrefix(input, strings.TrimSuffix(got, "…")) {
			t.Errorf("truncateForMessage(%d ASCII + kanji) = %q: not a prefix of its input", lead, got)
		}
	}
}
