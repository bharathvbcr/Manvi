package xai

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// TestHostedXAIDoesNotExecuteFencedProse: xAI parses tool calls itself, so a
// fenced JSON object in its text is the model showing an example. It used to
// inherit the local server's text recovery and ran it.
func TestHostedXAIDoesNotExecuteFencedProse(t *testing.T) {
	const prose = "Here is how a call looks:\\n```json\\n{\\\"name\\\": \\\"read_file\\\", \\\"arguments\\\": {\\\"path\\\": \\\"x\\\"}}\\n```"
	body := `data: {"choices":[{"delta":{"content":"` + prose + `"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":9}}` + "\n\ndata: [DONE]\n\n"
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}
	if calls := resp.Message.ToolCalls(); len(calls) != 0 {
		t.Fatalf("hosted xAI turned a fenced prose example into a call: %s stop=%s format=%q",
			calls[0].Arguments, resp.StopReason, resp.Decoding.FallbackFormat)
	}
	if resp.StopReason == llm.StopToolUse || !strings.Contains(resp.Message.Text(), "```json") {
		t.Fatalf("stop=%s text=%q", resp.StopReason, resp.Message.Text())
	}
}

// TestTruncatedXAIStreamIsNotASuccess: the same EOF rule through the hosted adapter.
func TestTruncatedXAIStreamIsNotASuccess(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"read_file"}}]}}]}` + "\n\n"
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	if _, resp, err := adaptertest.Drain(stream); err == nil {
		t.Fatalf("truncated stream dispatched %d call(s)", len(resp.Message.ToolCalls()))
	}
}
