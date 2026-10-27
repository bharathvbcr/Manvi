package session_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/openaicompat"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// emptyReadFileHistory is the journal of a turn in which read_file returned an
// empty file: ToolResultData{Text:""}, which the projection renders as a text
// block with no text. Journals on disk already hold this shape, so the encoders
// are where it has to be made valid.
func emptyReadFileHistory(t *testing.T) []llm.Message {
	t.Helper()
	log := session.NewLog()
	appends := []struct {
		typ     session.Type
		payload any
	}{
		{session.TurnStart, nil},
		{session.UserMessage, session.MessageData{Message: llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "read empty.txt"}}}}},
		{session.AssistantMessage, session.MessageData{Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.TextBlock{Text: ""}, // opened, never got a delta
			llm.ToolCallBlock{ID: "call_1", Name: "devcouncil_read_file", Arguments: json.RawMessage(`{"path":"empty.txt"}`)},
		}}}},
		{session.ToolResult, session.ToolResultData{ToolCallID: "call_1", Text: ""}},
	}
	for _, a := range appends {
		if _, err := log.Append(a.typ, a.payload); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := log.DeriveMessages()
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

const anthropicDone = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":5}}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

event: message_stop
data: {"type":"message_stop"}

`

func TestAnEmptyReadFileRoundTripsToAValidAnthropicRequest(t *testing.T) {
	server := adaptertest.NewServer(t, anthropicDone)
	adapter := anthropic.New(server.URL, adaptertest.Secret("sk-FIXTURE-not-a-real-key"))
	stream, err := adapter.Stream(adaptertest.Ctx(), llm.Request{Model: "claude-opus-5-5", MaxTokens: 100, Messages: emptyReadFileHistory(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatal(err)
	}
	body := server.Requests()[0]
	if strings.Contains(body, `{"type":"text"}`) || strings.Contains(body, `"text":""`) {
		t.Errorf("request carries a text block with no text:\n%s", body)
	}
	if !strings.Contains(body, `{"type":"tool_result","tool_use_id":"call_1"}`) {
		t.Errorf("empty read_file did not become a bare tool_result:\n%s", body)
	}
}

func TestAnEmptyReadFileRoundTripsToAValidOpenAICompatRequest(t *testing.T) {
	server := adaptertest.NewServer(t, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	adapter := openaicompat.New(openaicompat.Options{
		Name: "test-provider", BaseURL: server.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	stream, err := adapter.Stream(context.Background(), llm.Request{Model: "m", Messages: emptyReadFileHistory(t)})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatal(err)
	}
	body := server.Requests()[0]
	if !strings.Contains(body, `{"role":"tool","tool_call_id":"call_1","content":""}`) {
		t.Errorf("empty read_file did not become a tool message with content \"\":\n%s", body)
	}
}
