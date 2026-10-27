package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// TestEmptyToolResultWireJSON pins the exact bytes of a tool_result with nothing
// to say.
func TestEmptyToolResultWireJSON(t *testing.T) {
	cases := []struct {
		name  string
		block llm.ContentBlock
		want  string
	}{
		{"tool_result empty text", llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}},
			`{"type":"tool_result","tool_use_id":"toolu_1"}`},
		{"tool_result no content", llm.ToolResultBlock{ToolCallID: "toolu_1"}, `{"type":"tool_result","tool_use_id":"toolu_1"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, _, err := toWireMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{c.block}})
			if err != nil {
				t.Fatal(err)
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

// TestAnEmptyToolResultIsSentWithoutContent: ToolResultBlockParam.content is
// optional, TextBlockParam.text has minLength 1. An empty read_file result must
// reach the wire as a tool_result with no content, not a text block with no text.
func TestAnEmptyToolResultIsSentWithoutContent(t *testing.T) {
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCallBlock{ID: "toolu_1", Name: "t", Arguments: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}}}},
	})
	if !strings.Contains(body, `{"type":"tool_result","tool_use_id":"toolu_1"}`) {
		t.Errorf("empty tool result was not sent as a bare tool_result:\n%s", body)
	}
	assertWireIsValid(t, body)
}

// TestEmptyTextNeverReachesTheWire covers every place an empty text block can
// come from: a tool result whose other content survives, an assistant message
// that mixes it with a call, and a whole assistant turn that said nothing.
func TestEmptyTextNeverReachesTheWire(t *testing.T) {
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.TextBlock{Text: ""},
			llm.ToolCallBlock{ID: "toolu_1", Name: "t", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{
			llm.TextBlock{Text: ""},
			llm.ImageBlock{MediaType: "image/png", Data: []byte("x")},
		}}}},
		// A turn the model spent saying nothing: after the empty text is
		// dropped it has no content, and the API rejects an empty non-final
		// message, so the message goes with it. Consecutive same-role turns are
		// combined by the API, so the history stays well formed.
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "continue"}}},
	})
	assertWireIsValid(t, body)
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	var roles []string
	for _, m := range req.Messages {
		roles = append(roles, m.Role)
	}
	if got := strings.Join(roles, ","); got != "user,assistant,user,user" {
		t.Errorf("roles = %s, want the empty assistant turn dropped (user,assistant,user,user)", got)
	}
	if first := req.Messages[1].Content; len(first) != 1 || first[0].Type != "tool_use" {
		t.Errorf("assistant content = %+v, want only the tool_use", first)
	}
}

// TestAnEmptyUserMessageIsRefusedNotSent: a user turn with no text is the
// caller's mistake, not model output, and silently dropping it would change who
// spoke last.
func TestAnEmptyUserMessageIsRefusedNotSent(t *testing.T) {
	adapter, server := adapterFor(t, minimalStream)
	_, err := adapter.Stream(adaptertest.Ctx(), llm.Request{Model: "claude-opus-5-5", MaxTokens: 100,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}}}})
	if err == nil {
		t.Fatal("a user message with only empty text was accepted")
	}
	if n := len(server.Requests()); n != 0 {
		t.Errorf("%d request(s) sent for a message that cannot be encoded", n)
	}
}

// TestAnAssistantTextBlockThatNeverGotADeltaIsNotReplayed: the decoder keeps a
// block that opened and received nothing as TextBlock{""}, and the journal
// holds it. The encoder drops it.
func TestAnAssistantTextBlockThatNeverGotADeltaIsNotReplayed(t *testing.T) {
	stream := strings.Replace(minimalStream, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

`, "", 1)
	adapter, _ := adapterFor(t, stream)
	s, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(s)
	if err != nil {
		t.Fatal(err)
	}
	if tb, ok := resp.Message.Content[0].(llm.TextBlock); !ok || tb.Text != "" {
		t.Fatalf("settled content = %#v, want one empty text block (the journal's shape)", resp.Message.Content)
	}
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		resp.Message,
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "again"}}},
	})
	assertWireIsValid(t, body)
}

// An error result that said nothing is sent with a placeholder: the live API is
// reported to refuse is_error:true with no content (anthropics/claude-agent-sdk-python#248;
// not in the reference). A non-error result stays content-less.
func TestAnErrorToolResultWithNothingToSayCarriesAPlaceholder(t *testing.T) {
	const want = `{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"(no output)"}],"is_error":true}`
	for name, content := range map[string][]llm.ContentBlock{
		"empty text":    {llm.TextBlock{Text: ""}},
		"whitespace":    {llm.TextBlock{Text: "\n"}},
		"no content":    nil,
		"nothing sends": {llm.TextBlock{Text: ""}, llm.TextBlock{Text: " "}},
	} {
		t.Run(name, func(t *testing.T) {
			msg, _, err := toWireMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
				llm.ToolResultBlock{ToolCallID: "toolu_1", IsError: true, Content: content}}})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(msg.Content[0])
			if string(got) != want {
				t.Errorf("wire JSON\n got %s\nwant %s", got, want)
			}
		})
	}
	// Content that exists is left alone.
	msg, _, err := toWireMessage(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
		llm.ToolResultBlock{ToolCallID: "toolu_1", IsError: true, Content: []llm.ContentBlock{llm.TextBlock{Text: "boom"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := json.Marshal(msg.Content[0]); strings.Contains(string(got), "(no output)") {
		t.Errorf("a real error message was replaced: %s", got)
	}
}

// Thinking that was cut off before its signature arrived (max_tokens inside the
// thinking block) cannot be replayed validly, so it is dropped; a signed block
// next to it is untouched.
func TestAnUnsignedThinkingBlockIsNotReplayed(t *testing.T) {
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "half a thou"}, // no signature
			llm.ReasoningBlock{Text: "", Signature: "SIG"},
			llm.TextBlock{Text: "answer"},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "next"}}},
		// Only an unsigned block: the whole assistant turn goes.
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "cut"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "again"}}},
	})
	assertWireIsValid(t, body)
	if strings.Contains(body, "half a thou") || strings.Contains(body, `"thinking":"cut"`) {
		t.Errorf("an unsigned thinking block reached the wire:\n%s", body)
	}
	if strings.Count(body, `"type":"thinking"`) != 1 {
		t.Errorf("want exactly the signed thinking block:\n%s", body)
	}
	if strings.Count(body, `"role":"assistant"`) != 1 {
		t.Errorf("the assistant turn holding only unsigned thinking was not dropped:\n%s", body)
	}
}

// A text block with only whitespace is dropped, and text with any other
// content is sent untrimmed, byte for byte. The whitespace rule is third-party
// evidence (microsoft/vscode#307807, anthropics/claude-code#23440), not in the
// reference.
func TestWhitespaceOnlyTextIsDroppedAndOtherTextIsUntouched(t *testing.T) {
	body := sendHistory(t, []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.TextBlock{Text: "\n\n"},
			llm.TextBlock{Text: " \t "},
			llm.TextBlock{Text: "  keep me\n"},
			llm.ToolCallBlock{ID: "toolu_1", Name: "t", Arguments: json.RawMessage(`{}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "\n"}}}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "\n\n"}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "next"}}},
	})
	assertWireIsValid(t, body)
	if !strings.Contains(body, `{"type":"text","text":"  keep me\n"}`) {
		t.Errorf("non-whitespace text was altered:\n%s", body)
	}
	if !strings.Contains(body, `{"type":"tool_result","tool_use_id":"toolu_1"}`) {
		t.Errorf("a whitespace-only tool output was not sent as a bare tool_result:\n%s", body)
	}
	if strings.Count(body, `"role":"assistant"`) != 1 {
		t.Errorf("the whitespace-only assistant turn was not dropped:\n%s", body)
	}
}

// MarshalJSON owns only thinking, signature and data on the block types that
// require them. It must never write a key twice, and must leave every other
// field as the plain marshal wrote it.
func TestWireBlockMarshalNeverDuplicatesAKey(t *testing.T) {
	src := &wireSource{Type: "base64", MediaType: "image/png", Data: "AAAA"}
	blocks := []wireBlock{
		{Type: BlockThinking, Thinking: "t", Signature: "s", Text: "x", ID: "i", Name: "n", Input: json.RawMessage(`{"a":1}`),
			ToolUseID: "u", Content: []wireBlock{{Type: BlockText, Text: "c"}}, IsError: true, Source: src, Data: "d"},
		{Type: BlockRedactedThinking, Data: "d", Thinking: "t", Signature: "s"},
		{Type: BlockText, Text: "t", Thinking: "must-not-appear-elsewhere"},
		{Type: BlockToolUse, ID: "i", Name: "n", Input: json.RawMessage(`{}`)},
		{Type: "tool_result", ToolUseID: "u", IsError: true, Content: []wireBlock{{Type: BlockThinking}}},
		{Type: "image", Source: src},
	}
	for i, b := range blocks {
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		if _, err := dec.Token(); err != nil { // opening brace
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				t.Fatal(err)
			}
			key, _ := keyTok.(string)
			if seen[key] {
				t.Errorf("block %d: duplicate key %q in %s", i, key, raw)
			}
			seen[key] = true
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatal(err)
			}
		}
	}
}
