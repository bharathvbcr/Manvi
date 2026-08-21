package xai

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/llm"
	"manvi/llm/adaptertest"
)

// happyStream interleaves two tool calls on purpose. Keying fragments by
// arrival order rather than by the wire's index concatenates one call's
// arguments onto the other, and the result still parses — which is exactly the
// kind of failure that reaches production.
const happyStream = `data: {"choices":[{"delta":{"reasoning_content":"considering"}}]}

data: {"choices":[{"delta":{"content":"Reading "}}]}

data: {"choices":[{"delta":{"content":"two files."}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read_file"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"read_file"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"a.go\"}"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"b.go\"}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":31,"completion_tokens":64}}

data: [DONE]

`

func adapterFor(t *testing.T, body string) (*Adapter, *adaptertest.Server) {
	t.Helper()
	server := adaptertest.NewServer(t, body)
	return New(server.URL, adaptertest.Secret("xai-test-key-value")), server
}

func request() llm.Request {
	return llm.Request{
		Model:    "grok-4.3",
		System:   "You are the builder.",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "read them"}}}},
		Tools: []llm.ToolSchema{{
			Name: "read_file", InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
		MaxTokens: 2048,
		Effort:    EffortLow,
	}
}

func TestAFullTurnDecodes(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatal(err)
	}

	if got := adaptertest.TextOf(chunks); got != "Reading two files." {
		t.Errorf("text = %q", got)
	}
	if got := adaptertest.ReasoningOf(chunks); got != "considering" {
		t.Errorf("reasoning = %q", got)
	}
	if resp.StopReason != llm.StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	if resp.Usage.InputTokens != 31 || resp.Usage.OutputTokens != 64 {
		t.Errorf("usage = %+v; a usage-only trailing frame must still be read", resp.Usage)
	}

	calls := resp.Message.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d, want 2", len(calls))
	}
	want := map[llm.CallID]string{"call_a": "a.go", "call_b": "b.go"}
	for _, call := range calls {
		var args struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			t.Fatalf("call %s arguments did not reassemble: %v (%s)", call.ID, err, call.Arguments)
		}
		if args.Path != want[call.ID] {
			t.Errorf("call %s path = %q, want %q — interleaved fragments were mixed between calls",
				call.ID, args.Path, want[call.ID])
		}
	}

	// The request shape.
	var body map[string]any
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "You are the builder." {
		t.Errorf("the system prompt must lead as a system-role message: %v", first)
	}
	if body["reasoning_effort"] != EffortLow {
		t.Errorf("reasoning_effort = %v", body["reasoning_effort"])
	}
	if body["max_completion_tokens"] != float64(2048) {
		t.Errorf("max_completion_tokens = %v; max_tokens is deprecated", body["max_completion_tokens"])
	}
	opts, _ := body["stream_options"].(map[string]any)
	if opts["include_usage"] != true {
		t.Error("usage must be requested, or the turn cannot be costed afterwards")
	}
	if got := server.Headers[0].Get("Authorization"); got != "Bearer xai-test-key-value" {
		t.Errorf("Authorization = %q", got)
	}
}

// TestReasoningEffortIsRefusedOnModelsThatLackIt: OpenAI-compatible is a claim
// about request shape, not about which parameters a model accepts.
func TestReasoningEffortIsRefusedOnModelsThatLackIt(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Model = "grok-4.6"
	if _, err := adapter.Stream(adaptertest.Ctx(), req); err == nil {
		t.Fatal("reasoning_effort was sent to a model that does not document it")
	}
	if len(server.Requests) != 0 {
		t.Fatal("the request reached the network")
	}
}

// TestTruncatedToolArgumentsAreNotDispatchedButDoNotKillTheTurn.
//
// The safety property is unchanged: a call whose arguments were cut off is
// never surfaced as runnable. What changed is the blast radius. It used to fail
// the whole request, which threw away every completed step of the turn over a
// response that merely ran out of room. Now it is reported as malformed, and
// the loop tells the model so it can ask again with less.
func TestTruncatedToolArgumentsAreNotDispatchedButDoNotKillTheTurn(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"write_file","arguments":"{\"path\":\"a.go\",\"content\":\"pack"}}]}}]}

data: [DONE]

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("a truncated response failed the whole request: %v", err)
	}
	if calls := resp.Message.ToolCalls(); len(calls) != 0 {
		t.Fatalf("a call with cut-off arguments was surfaced as runnable: %+v", calls)
	}
	if len(resp.Malformed) != 1 {
		t.Fatalf("the truncation was not reported: %+v", resp.Malformed)
	}
	if resp.Malformed[0].Name != "write_file" {
		t.Errorf("malformed call name = %q", resp.Malformed[0].Name)
	}
	if !strings.Contains(resp.Malformed[0].Reason, "cut off") {
		t.Errorf("reason does not say what happened: %q", resp.Malformed[0].Reason)
	}
	if resp.StopReason == llm.StopToolUse {
		t.Error("a response with no runnable calls reported tool use")
	}
}

// TestAToolCallWithNoNameIsNotDispatched: dispatching it would look up the empty
// tool name and report "unknown tool", hiding a decode failure as a model error.
func TestAToolCallWithNoNameIsNotDispatched(t *testing.T) {
	body := `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"arguments":"{}"}}]}}]}

data: [DONE]

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	_, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if calls := resp.Message.ToolCalls(); len(calls) != 0 {
		t.Fatal("a nameless tool call was surfaced as runnable")
	}
	if len(resp.Malformed) != 1 {
		t.Fatalf("the nameless call was not reported: %+v", resp.Malformed)
	}
}

// TestAnErrorFrameFailsTheTurn.
func TestAnErrorFrameFailsTheTurn(t *testing.T) {
	body := `data: {"choices":[{"delta":{"content":"partial"}}]}

data: {"error":{"message":"rate limited","code":429}}

`
	adapter, _ := adapterFor(t, body)
	stream, _ := adapter.Stream(adaptertest.Ctx(), request())
	_, _, err := adaptertest.Drain(stream)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("err = %v, want the provider's message", err)
	}
}

// TestToolResultsBecomeOneMessageEach is a shape difference, not a spelling
// one: flattening several results into one message pairs every result with the
// first call.
func TestToolResultsBecomeOneMessageEach(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages,
		llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: "call_a", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
			llm.ToolCallBlock{ID: "call_b", Name: "read_file", Arguments: json.RawMessage(`{"path":"b.go"}`)},
		}},
		llm.Message{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolCallID: "call_a", Content: []llm.ContentBlock{llm.TextBlock{Text: "contents of a"}}},
			llm.ToolResultBlock{ToolCallID: "call_b", Content: []llm.ContentBlock{llm.TextBlock{Text: "contents of b"}}},
		}},
	)
	stream, err := adapter.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	adaptertest.Drain(stream)

	var body struct {
		Messages []struct {
			Role       string `json:"role"`
			Content    string `json:"content"`
			ToolCallID string `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(server.Requests[0]), &body); err != nil {
		t.Fatal(err)
	}
	pairs := map[string]string{}
	for _, m := range body.Messages {
		if m.Role == "tool" {
			pairs[m.ToolCallID] = m.Content
		}
	}
	if len(pairs) != 2 || pairs["call_a"] != "contents of a" || pairs["call_b"] != "contents of b" {
		t.Fatalf("tool results = %v, want one message per result naming its own call", pairs)
	}
}

// TestUnstrippedReasoningIsRefused: this provider cannot replay it, so
// PrepareHistory removes it. One arriving here means it did not, and sending it
// as text would silently change the prompt.
func TestUnstrippedReasoningIsRefused(t *testing.T) {
	adapter, _ := adapterFor(t, happyStream)
	req := request()
	req.Messages = append(req.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.ContentBlock{llm.ReasoningBlock{Text: "leftover thinking"}},
	})
	if _, err := adapter.Stream(adaptertest.Ctx(), req); err == nil {
		t.Fatal("an unstripped reasoning block was sent")
	}
}
