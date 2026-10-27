package anthropic

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// A message that is a cut-off call and nothing else settles with no content; the
// next request, which carries it, has to be encodable.
func TestACutCallThatIsTheOnlyBlockLeavesAnEncodableHistory(t *testing.T) {
	resp, err := settle(t, startFrame+toolFrames(0, "toolu_1", `{"path":"a`, true)+finish("max_tokens"))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Message.Content) != 0 {
		t.Fatalf("content = %#v, want none", resp.Message.Content)
	}
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		resp.Message,
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "your call was cut off"}}},
	})
	assertWireIsValid(t, body)
}

// redacted_thinking and signed thinking go through the decoder and back out
// with their opaque values intact, and the empty text block between them is not
// replayed.
func TestThinkingAndRedactedThinkingSurviveAStreamRoundTrip(t *testing.T) {
	resp, err := settle(t, startFrame+`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"AAA"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"BBB"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"redacted_thinking","data":"ENC=="}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"text","text":""}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

`+finish("end_turn"))
	if err != nil {
		t.Fatal(err)
	}
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		resp.Message,
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "next"}}},
	})
	assertWireIsValid(t, body)
	for _, want := range []string{`{"type":"thinking","thinking":"","signature":"AAABBB"}`, `{"type":"redacted_thinking","data":"ENC=="}`} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s in:\n%s", want, body)
		}
	}
}
