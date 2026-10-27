package openaicompat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
)

// The chat-completions wire requires `content` on a role:"tool" message, so an
// empty tool output (a command that printed nothing, an empty file) is sent as
// content:"" rather than with the key dropped. Every other role keeps omitting
// an empty content: an assistant message that only carries tool_calls has none.
func TestAToolMessageAlwaysCarriesContent(t *testing.T) {
	cases := []struct {
		name string
		msg  llm.Message
		want string
	}{
		{"empty tool output", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolCallID: "call_1", Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}},
		}}, `[{"role":"tool","tool_call_id":"call_1","content":""}]`},
		{"tool output without content blocks", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolCallID: "call_1"},
		}}, `[{"role":"tool","tool_call_id":"call_1","content":""}]`},
		{"non-empty tool output", llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolCallID: "call_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "out"}}},
		}}, `[{"role":"tool","tool_call_id":"call_1","content":"out"}]`},
		{"assistant with only a call has no content", llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: "call_1", Name: "t", Arguments: json.RawMessage(`{}`)},
		}}, `[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"t","arguments":"{}"}}]}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msgs, err := toWireMessages(c.msg, "xai")
			if err != nil {
				t.Fatal(err)
			}
			got, err := json.Marshal(msgs)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != c.want {
				t.Errorf("wire JSON\n got %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestAnEmptyToolOutputReachesTheServerWithContent drives the same case through
// Stream, so the assertion is on the request body a server would actually see.
func TestAnEmptyToolOutputReachesTheServerWithContent(t *testing.T) {
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		body = string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()
	adapter := New(Options{
		Name: "test-provider", BaseURL: server.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	stream, err := adapter.Stream(context.Background(), llm.Request{Model: "m", Messages: []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "read it"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.ToolCallBlock{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultBlock{ToolCallID: "call_1", Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := stream.Next(); err != nil {
			break
		}
	}
	_ = stream.Close()
	if !strings.Contains(body, `{"role":"tool","tool_call_id":"call_1","content":""}`) {
		t.Errorf("the tool message sent to the server has no content:\n%s", body)
	}
}

// The anthropic adapter settles a text block that never received a delta as
// TextBlock{""}, so a session that moves to this adapter holds assistant turns
// that say nothing. They are omitted, as the anthropic encoder omits them; a
// user message with nothing in it stays refused.
func TestAnAssistantTurnOfOnlyEmptyTextIsOmittedAndAnEmptyUserTurnIsRefused(t *testing.T) {
	msgs, err := toWireMessages(llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}}, "xai")
	if err != nil {
		t.Fatalf("an assistant turn of only empty text was refused: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("messages = %+v, want none", msgs)
	}
	if _, err := toWireMessages(llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: ""}}}, "xai"); err == nil {
		t.Error("a user message with no content was accepted")
	}
}
