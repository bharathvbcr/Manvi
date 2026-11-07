package anthropic

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
)

// sentRequest streams req through a fixture server and returns the body and
// headers it received.
func sentRequest(t *testing.T, a *Adapter, server *adaptertest.Server, req llm.Request) (map[string]json.RawMessage, string, http.Header) {
	t.Helper()
	stream, err := a.Stream(adaptertest.Ctx(), req)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := adaptertest.Drain(stream); err != nil {
		t.Fatal(err)
	}
	bodies := server.Requests()
	if len(bodies) != 1 {
		t.Fatalf("requests = %d, want 1", len(bodies))
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(bodies[0]), &fields); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	return fields, bodies[0], server.Headers()[0]
}

// A request that carries tools asks the API to clear old tool results, under the
// beta and with the strategy the Context editing page documents. This is the
// server-side replacement for the client-side compaction an Anthropic
// conversation no longer does (agent/compaction.go says why).
func TestARequestWithToolsAsksForServerSideToolResultClearing(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	fields, _, headers := sentRequest(t, adapter, server, request())

	if got := headers.Get("anthropic-beta"); got != "context-management-2025-06-27" {
		t.Errorf("anthropic-beta = %q, want context-management-2025-06-27", got)
	}
	var cm struct {
		Edits []struct {
			Type    string `json:"type"`
			Trigger struct {
				Type  string
				Value int
			} `json:"trigger"`
			Keep struct {
				Type  string
				Value int
			} `json:"keep"`
			ClearAtLeast struct {
				Type  string
				Value int
			} `json:"clear_at_least"`
		} `json:"edits"`
	}
	raw, ok := fields["context_management"]
	if !ok {
		t.Fatal("a request with tools carries no context_management")
	}
	if err := json.Unmarshal(raw, &cm); err != nil {
		t.Fatal(err)
	}
	if len(cm.Edits) != 1 || cm.Edits[0].Type != "clear_tool_uses_20250919" {
		t.Fatalf("edits = %s, want one clear_tool_uses_20250919", raw)
	}
	e := cm.Edits[0]
	if e.Trigger.Type != "input_tokens" || e.Trigger.Value <= 0 ||
		e.ClearAtLeast.Type != "input_tokens" || e.ClearAtLeast.Value <= 0 ||
		e.Keep.Type != "tool_uses" || e.Keep.Value != 3 {
		t.Errorf("thresholds are malformed: %s", raw)
	}
	// Below the model's window by the reserved output and the overhead, which is
	// where agent.Budget puts its own threshold, and not the server default of
	// 100,000 that would clear results a million-token window had room for.
	if e.Trigger.Value <= 100_000 {
		t.Errorf("trigger %d is at or under the server default; a 1M window would clear needlessly", e.Trigger.Value)
	}
	if e.ClearAtLeast.Value >= e.Trigger.Value {
		t.Errorf("clear_at_least %d must be a fraction of the trigger %d", e.ClearAtLeast.Value, e.Trigger.Value)
	}
}

// Without tools there are no tool results to clear, and the request says
// nothing about it.
func TestARequestWithoutToolsCarriesNoContextManagement(t *testing.T) {
	adapter, server := adapterFor(t, happyStream)
	req := request()
	req.Tools = nil
	fields, _, _ := sentRequest(t, adapter, server, req)
	if _, ok := fields["context_management"]; ok {
		t.Error("context_management was sent on a request with no tools")
	}
}

// The docs say not to combine tool-result clearing with threshold compaction,
// and the preserved-thinking controls are an opt-in this adapter has chosen not
// to use: no drop_block, no automatic re-spend. `between_tools` is Sonnet 5.5's
// way to turn thinking off and takes no other field, so a conversation that
// sent it would have no block_binding to protect it. None of these may appear
// for any model in the catalogue, in any thinking mode.
func TestNoRequestSendsCompactionDropBlockOrBetweenTools(t *testing.T) {
	for model := range catalogue {
		for _, mode := range []ThinkingMode{ThinkingAdaptive, ThinkingDefault} {
			adapter, server := adapterFor(t, happyStream)
			adapter.Thinking = mode
			req := request()
			req.Model = model
			_, body, headers := sentRequest(t, adapter, server, req)
			for _, banned := range []string{
				"compact_20260112", "between_tools", "block_binding", "drop_block", "prefix_mismatch_behavior",
			} {
				if strings.Contains(body, banned) {
					t.Errorf("%s/%q: request body carries %q:\n%.400s", model, mode, banned, body)
				}
			}
			beta := headers.Get("anthropic-beta")
			for _, banned := range []string{"compact-2026-01-12", "thinking-binding-controls-2026-08-01"} {
				if strings.Contains(beta, banned) {
					t.Errorf("%s/%q: anthropic-beta carries %q", model, mode, banned)
				}
			}
		}
	}
}

// A model switch is not an edit. The API drops, unbilled, the blocks the target
// cannot read, and stripping them client-side is itself an edit of earlier
// turns that removes reasoning the target could have read. So reasoning crosses
// between reasoning models, and is kept from a model that did not write it.
func TestAModelSwitchDoesNotStripReasoningTheAPIWouldHandle(t *testing.T) {
	history := []llm.Message{{
		Role: llm.RoleAssistant,
		Content: []llm.ContentBlock{
			llm.ReasoningBlock{Text: "internal", Signature: "sig"},
			llm.TextBlock{Text: "answer"},
		},
		Provenance: &llm.AssistantProvenance{Provider: Name, Model: "claude-sonnet-5-5"},
	}}
	for _, target := range []string{"claude-opus-5-5", "claude-opus-4-8", "claude-sonnet-5-5"} {
		out, drops := llm.PrepareHistoryFor(history, &Adapter{}, target)
		if len(drops) != 0 || len(out[0].Content) != 2 {
			t.Errorf("sonnet-5-5 -> %s: reasoning was stripped on a model switch (drops=%v)", target, drops)
		}
	}
	// A model that does not reason is sent no thinking configuration, and
	// history full of thinking blocks is not something the docs promise it
	// takes. That one still gets history without them.
	out, drops := llm.PrepareHistoryFor(history, &Adapter{}, "claude-haiku-4-5")
	if len(drops) != 1 || len(out[0].Content) != 1 {
		t.Errorf("a non-reasoning target must still be handed history without thinking (drops=%v)", drops)
	}
}
