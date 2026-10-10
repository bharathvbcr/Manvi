package adaptertest_test

import (
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/gemini"
	"github.com/bharathvbcr/Manvi/manvi/llm/xai"
)

// TestEveryDecoderRefusesATruncatedToolCall runs one rule against every wire:
// a stream that ends after a tool call opened, before its terminal event, is
// an error and dispatches nothing — never `{}`.
func TestEveryDecoderRefusesATruncatedToolCall(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	user := []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}}}
	for _, c := range []adaptertest.TruncationCase{
		{
			Name: "anthropic",
			Provider: func(u string) llm.Provider {
				return anthropic.New(u, adaptertest.Secret("k"))
			},
			Request: llm.Request{Model: "claude-opus-5", MaxTokens: 100, Messages: user,
				Tools: []llm.ToolSchema{{Name: "bash", InputSchema: schema}}},
			Body: "event: content_block_start\n" +
				`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t1","name":"bash"}}` + "\n\n",
		},
		{
			Name: "gemini",
			Provider: func(u string) llm.Provider {
				return gemini.New(u, adaptertest.Secret("k"))
			},
			Request: llm.Request{Model: "gemini-3.7-flash", Messages: user,
				Tools: []llm.ToolSchema{{Name: "bash", InputSchema: schema}}},
			Body: "event: step.start\n" +
				`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"f1","name":"bash","arguments":{}}}` + "\n\n",
		},
		{
			Name: "openaicompat (xai)",
			Provider: func(u string) llm.Provider {
				return xai.New(u, adaptertest.Secret("k"))
			},
			Request: llm.Request{Model: "grok-4.3", Messages: user,
				Tools: []llm.ToolSchema{{Name: "bash", InputSchema: schema}}},
			Body: `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"bash"}}]}}]}` + "\n\n",
		},
	} {
		t.Run(c.Name, func(t *testing.T) { adaptertest.AssertTruncatedCallIsNeverDispatched(t, c) })
	}
}
