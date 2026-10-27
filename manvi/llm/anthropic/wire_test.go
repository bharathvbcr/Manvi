package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// minimalStream is the smallest complete message: one text block with a delta.
const minimalStream = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

// sendHistory sends msgs through the public Stream path against a fixture
// server and returns the request body that reached the wire.
func sendHistory(t *testing.T, msgs []llm.Message) string {
	t.Helper()
	adapter, server := adapterFor(t, minimalStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), llm.Request{Model: "claude-opus-5-5", Messages: msgs, MaxTokens: 100})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	reqs := server.Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	return reqs[0]
}

// assertWireIsValid checks the documented Messages API constraints on a request
// body that this package cannot enforce by construction: a text block carries
// non-empty text (TextBlockParam.text has minLength 1), a thinking block carries
// both "thinking" and "signature" (ThinkingBlockParam requires them), and a
// block of any other type carries no "thinking" field.
func assertWireIsValid(t *testing.T, body string) {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request body is not JSON: %v\n%s", err, body)
	}
	var check func(raw json.RawMessage, path string)
	check = func(raw json.RawMessage, path string) {
		var block map[string]json.RawMessage
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Errorf("%s: not an object: %s", path, raw)
			return
		}
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		switch kind {
		case "text":
			var text string
			if err := json.Unmarshal(block["text"], &text); err != nil || text == "" {
				t.Errorf("%s: text block without non-empty text: %s", path, raw)
			}
		case "thinking":
			for _, key := range []string{"thinking", "signature"} {
				if _, ok := block[key]; !ok {
					t.Errorf("%s: thinking block without %q: %s", path, key, raw)
				}
			}
		case "tool_result":
			if inner, ok := block["content"]; ok {
				var blocks []json.RawMessage
				if err := json.Unmarshal(inner, &blocks); err != nil || len(blocks) == 0 {
					t.Errorf("%s: tool_result content present but not a non-empty array: %s", path, raw)
				}
				for i, b := range blocks {
					check(b, fmt.Sprintf("%s.content[%d]", path, i))
				}
			}
		}
		if kind != "thinking" {
			if _, ok := block["thinking"]; ok {
				t.Errorf("%s: %s block gained a thinking field: %s", path, kind, raw)
			}
		}
	}
	for i, m := range req.Messages {
		if len(m.Content) == 0 {
			t.Errorf("message %d (%s) has no content blocks", i, m.Role)
		}
		for j, raw := range m.Content {
			check(raw, fmt.Sprintf("messages[%d].content[%d]", i, j))
		}
	}
}

// TestWireJSONOfEachBlockType pins the exact bytes of every block type this
// adapter sends. The thinking and redacted_thinking rows are the ones that
// matter: both fields are required by the API and an empty value is a value.
func TestWireJSONOfEachBlockType(t *testing.T) {
	cases := []struct {
		name  string
		block llm.ContentBlock
		want  string
	}{
		{"text", llm.TextBlock{Text: "hi"}, `{"type":"text","text":"hi"}`},
		{"thinking with text", llm.ReasoningBlock{Text: "why", Signature: "SIG"}, `{"type":"thinking","thinking":"why","signature":"SIG"}`},
		// display "omitted" is the default on current models: the block arrives
		// with thinking:"" and must go back with thinking:"".
		{"thinking omitted display", llm.ReasoningBlock{Text: "", Signature: "SIG"}, `{"type":"thinking","thinking":"","signature":"SIG"}`},
		{"redacted thinking", llm.ReasoningBlock{Redacted: true, Signature: "OPAQUE"}, `{"type":"redacted_thinking","data":"OPAQUE"}`},
		{"tool_use", llm.ToolCallBlock{ID: "toolu_1", Name: "t", Arguments: json.RawMessage(`{"a":1}`)}, `{"type":"tool_use","id":"toolu_1","name":"t","input":{"a":1}}`},
		{"tool_use without arguments", llm.ToolCallBlock{ID: "toolu_1", Name: "t"}, `{"type":"tool_use","id":"toolu_1","name":"t","input":{}}`},
		{"tool_result", llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "out"}}},
			`{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"out"}]}`},
		{"tool_result error", llm.ToolResultBlock{ToolCallID: "toolu_1", IsError: true, Content: []llm.ContentBlock{llm.TextBlock{Text: "boom"}}},
			`{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"boom"}],"is_error":true}`},
		{"image", llm.ImageBlock{MediaType: "image/png", Data: []byte("x")}, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"eA=="}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, err := toWireMessage(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{c.block}})
			if err != nil {
				t.Fatal(err)
			}
			if len(msg.Content) != 1 {
				t.Fatalf("content = %d blocks, want 1", len(msg.Content))
			}
			got, err := json.Marshal(msg.Content[0])
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("wire JSON\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestAThinkingBlockIsReplayedWithItsThinkingField is the audit repro for the
// 400 on every tool-using turn after step one: "Thinking blocks must be passed
// back unmodified and in their original order", and thinking is not optional.
func TestAThinkingBlockIsReplayedWithItsThinkingField(t *testing.T) {
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "", Signature: "SIG"},
			llm.ToolCallBlock{ID: "toolu_1", Name: "t", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "x"}}}}},
	})
	if !strings.Contains(body, `{"type":"thinking","thinking":"","signature":"SIG"}`) {
		t.Errorf("omitted-display thinking block was not replayed with its thinking key:\n%s", body)
	}
	assertWireIsValid(t, body)
}

// TestAThinkingBlockSurvivesAReceiveAndReplayUnchanged runs a thinking block
// through the decoder and back out, for both display modes, and compares the
// replayed values to what the server sent.
func TestAThinkingBlockSurvivesAReceiveAndReplayUnchanged(t *testing.T) {
	for _, c := range []struct {
		name, deltas, wantThinking string
	}{
		{"omitted display", ``, ""},
		{"summarized display", `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"a <b> & \"c\""}}

`, `a <b> & "c"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			stream := `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

` + c.deltas + `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"Eu8B"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"+/=="}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`
			adapter, _ := adapterFor(t, stream)
			s, err := adapter.Stream(adaptertest.Ctx(), request())
			if err != nil {
				t.Fatal(err)
			}
			_, resp, err := adaptertest.Drain(s)
			if err != nil {
				t.Fatal(err)
			}
			msg, err := toWireMessage(resp.Message)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(msg.Content[0])
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				Type      string  `json:"type"`
				Thinking  *string `json:"thinking"`
				Signature *string `json:"signature"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatal(err)
			}
			if got.Type != "thinking" || got.Thinking == nil || *got.Thinking != c.wantThinking ||
				got.Signature == nil || *got.Signature != "Eu8B+/==" {
				t.Errorf("replayed %s; want thinking %q and signature Eu8B+/==", raw, c.wantThinking)
			}
		})
	}
}
