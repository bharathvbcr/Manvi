package anthropic

import (
	"encoding/json"
	"os"
	"sort"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// Adversarial tests for lane w11-compaction (card ft-f7993787), adapter half.
// Shapes are compared with the saved Context editing page
// (scratchpad/w11/context-editing.md, "Configuration options for tool result
// clearing" and the "Advanced configuration" example).

// advBreak skips a test that demonstrates a confirmed break. Setting
// ADV_W11_BREAKS=1 runs it so the failure can be reproduced.
func advBreak(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("ADV_W11_BREAKS") != "" {
		return
	}
	t.Skip(msg)
}

func advKeys(raw json.RawMessage) []string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Every catalogue model, several max_tokens: the edit has exactly the
// documented field names and units, one edit, and the trigger leaves room for
// the answer inside the window.
func TestAdvW11EditShapeMatchesTheDocsForEveryModel(t *testing.T) {
	for model := range catalogue {
		c, _ := Capability(model)
		for _, maxTokens := range []int{0, 1, 1024, 8192, 64_000, c.MaxOutputTokens} {
			adapter, server := adapterFor(t, happyStream)
			req := request()
			req.Model = model
			req.MaxTokens = maxTokens
			fields, _, _ := sentRequest(t, adapter, server, req)

			var cm struct {
				Edits []json.RawMessage `json:"edits"`
			}
			if err := json.Unmarshal(fields["context_management"], &cm); err != nil {
				t.Fatalf("%s: %v", model, err)
			}
			if got := advKeys(fields["context_management"]); len(got) != 1 || got[0] != "edits" {
				t.Errorf("%s: context_management keys = %v, want [edits]", model, got)
			}
			if len(cm.Edits) != 1 {
				t.Fatalf("%s: %d edits, want 1", model, len(cm.Edits))
			}
			if got := advKeys(cm.Edits[0]); len(got) != 4 ||
				got[0] != "clear_at_least" || got[1] != "keep" || got[2] != "trigger" || got[3] != "type" {
				t.Errorf("%s: edit keys = %v, want clear_at_least, keep, trigger, type", model, got)
			}
			var e struct {
				Type    string `json:"type"`
				Trigger struct {
					Type  string `json:"type"`
					Value int    `json:"value"`
				} `json:"trigger"`
				Keep struct {
					Type  string `json:"type"`
					Value int    `json:"value"`
				} `json:"keep"`
				ClearAtLeast struct {
					Type  string `json:"type"`
					Value int    `json:"value"`
				} `json:"clear_at_least"`
			}
			if err := json.Unmarshal(cm.Edits[0], &e); err != nil {
				t.Fatal(err)
			}
			if e.Type != "clear_tool_uses_20250919" || e.Trigger.Type != "input_tokens" ||
				e.Keep.Type != "tool_uses" || e.ClearAtLeast.Type != "input_tokens" {
				t.Errorf("%s: wrong type names: %s", model, cm.Edits[0])
			}
			if e.Keep.Value < 1 || e.ClearAtLeast.Value < 1 || e.ClearAtLeast.Value >= e.Trigger.Value {
				t.Errorf("%s max=%d: bad values: %s", model, maxTokens, cm.Edits[0])
			}
			applied := adapter.AppliedMaxTokens(req)
			if applied <= c.ContextWindow/2 && e.Trigger.Value+applied > c.ContextWindow {
				t.Errorf("%s max=%d: trigger %d + answer %d exceeds the %d window: the server would not clear before the request is over-window",
					model, maxTokens, e.Trigger.Value, applied, c.ContextWindow)
			}
		}
	}
}

// A model the catalogue lacks (a gateway alias) gets the edit with the
// server's defaults for trigger and clear_at_least, and the JSON is valid.
func TestAdvW11UnknownModelEditIsValidJSONWithServerDefaults(t *testing.T) {
	edits := ContextEditsFor("some-gateway-alias", 4096)
	raw, err := json.Marshal(edits)
	if err != nil {
		t.Fatal(err)
	}
	var back []map[string]any
	if err := json.Unmarshal(raw, &back); err != nil || len(back) != 1 {
		t.Fatalf("%s %v", raw, err)
	}
	if _, ok := back[0]["trigger"]; ok {
		t.Errorf("unknown model got a trigger: %s", raw)
	}
	if _, ok := back[0]["clear_at_least"]; ok {
		t.Errorf("unknown model got clear_at_least: %s", raw)
	}
}

// The beta header goes on every request the adapter makes, tools or not. The
// docs say the header alone changes nothing; this pins that a toolless request
// carries the header but no context_management (a field on a request with
// nothing to clear is what the lane avoided).
func TestAdvW11ToollessRequestHeaderWithoutField(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Tools = nil
	fields, _, headers := sentRequest(t, adapter, server, req)
	if _, ok := fields["context_management"]; ok {
		t.Error("toolless request carries context_management")
	}
	if headers.Get("anthropic-beta") != "context-management-2025-06-27" {
		t.Errorf("anthropic-beta = %q", headers.Get("anthropic-beta"))
	}
}

// ChatOnly runs (agent.Config.ChatOnly) offer the model no tools but resume a
// history that may be full of tool calls and results. Client-side compaction
// has stood down on Anthropic, and the adapter sends context_management only
// when req.Tools is non-empty, so a chat-only conversation on Anthropic is
// never shrunk by either side. The check: history that holds tool results must
// carry the edit even when no tools are offered this step.
func TestAdvW11ToollessRequestWithToolHistoryStillAsksForClearing(t *testing.T) {
	advBreak(t, "BREAK: ContextEditsFor is only attached when len(req.Tools) > 0 (adapter.go buildRequest), but agent compaction stands down for the whole provider; a ChatOnly turn over a log with tool history is shrunk by nobody. Inferred: whether the API accepts context_management on a request without tools is not stated in the saved doc, whose every example has tools.")
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Tools = nil
	req.Messages = []llm.Message{
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "go"}}},
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			llm.ToolCallBlock{ID: "toolu_1", Name: "devcouncil_read_file", Arguments: json.RawMessage(`{}`)}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{
			llm.ToolResultBlock{ToolCallID: "toolu_1", Content: []llm.ContentBlock{llm.TextBlock{Text: "big output"}}}}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "now just chat"}}},
	}
	fields, _, _ := sentRequest(t, adapter, server, req)
	if _, ok := fields["context_management"]; !ok {
		t.Fatal("history with tool results, no tools offered: no context_management, and no client-side compaction either")
	}
}

// Responses that carry the context_management object (docs: "Context editing
// response", applied_edits with cleared_tool_uses and cleared_input_tokens)
// must decode, and the usage must be what the wire reports: input_tokens in
// the response is already post-edit.
func TestAdvW11ContextManagementInResponseDoesNotBreakDecodingOrUsage(t *testing.T) {
	const body = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":42,"cache_read_input_tokens":10},"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":8,"cleared_input_tokens":50000}]}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":8,"cleared_input_tokens":50000}]},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`
	adapter, _ := adapterFor(t, body)
	stream, err := adapter.Stream(adaptertest.Ctx(), request())
	if err != nil {
		t.Fatal(err)
	}
	chunks, resp, err := adaptertest.Drain(stream)
	if err != nil {
		t.Fatalf("a response carrying context_management broke decoding: %v", err)
	}
	if adaptertest.TextOf(chunks) != "ok" || resp.StopReason != llm.StopEndTurn {
		t.Errorf("text=%q stop=%q", adaptertest.TextOf(chunks), resp.StopReason)
	}
	if resp.Usage.InputTokens != 52 || resp.Usage.OutputTokens != 5 || resp.Usage.CacheReadTokens != 10 {
		t.Errorf("usage = %+v, want 52 in (42 + 10 cache read), 5 out", resp.Usage)
	}
}

// ReplayableOn matrix after the lane's change.
func TestAdvW11ReplayableOnMatrix(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"claude-opus-5-5", "claude-opus-5-5", true},
		{"claude-sonnet-5-5", "claude-opus-5-5", true},
		{"claude-opus-5-5", "claude-opus-4-8", true},
		{"claude-opus-5-5", "claude-haiku-4-5", false}, // target does not reason
		{"claude-opus-5-5", "not-a-model", false},      // unknown target
		{"claude-opus-5-5", "", false},
		{"", "claude-opus-5-5", false},
		{"", "", false},
		{"claude-haiku-4-5", "claude-haiku-4-5", true},
	}
	for _, c := range cases {
		if got := ReplayableOn(c.from, c.to); got != c.want {
			t.Errorf("ReplayableOn(%q,%q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}

// A message produced by Anthropic and replayed to another provider's adapter
// loses its Anthropic reasoning and replay state, whatever the lane did to
// ReplayableOn: PrepareHistoryFor consults the replayer only when the
// providers match.
func TestAdvW11CrossProviderSwitchStillStripsAnthropicThinking(t *testing.T) {
	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "t", Signature: "sig"},
			llm.ReasoningBlock{Redacted: true, Signature: "opaque"},
			llm.TextBlock{Text: "answer"},
		},
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: "claude-opus-5-5", ReplayState: []byte(`{"x":1}`)},
	}}
	for _, target := range []string{"xai", "gemini", "local", "openai-compatible"} {
		out, drops := llm.PrepareHistoryFor(history, advForeign{name: target}, "any-model")
		if len(drops) != 1 {
			t.Fatalf("%s: drops = %v", target, drops)
		}
		for _, b := range out[0].Content {
			if b.Kind() == llm.KindReasoning {
				t.Errorf("%s received an Anthropic reasoning block: %+v", target, b)
			}
		}
		if len(out[0].Provenance.ReplayState) != 0 {
			t.Errorf("%s received Anthropic replay state", target)
		}
	}
	// Even a foreign adapter that claims everything is replayable is not asked.
	out, _ := llm.PrepareHistoryFor(history, advForeign{name: "xai", yes: true}, "m")
	if len(out[0].Content) != 1 {
		t.Errorf("a foreign adapter that says replayable still received %d blocks", len(out[0].Content))
	}
}

// advForeign is a minimal non-Anthropic provider.
type advForeign struct {
	llm.Provider
	name string
	yes  bool
}

func (f advForeign) Name() string                             { return f.name }
func (f advForeign) ReplayableOn(from, to string) bool        { return f.yes }
func (f advForeign) Capability(string) (llm.Capability, bool) { return llm.Capability{}, false }
