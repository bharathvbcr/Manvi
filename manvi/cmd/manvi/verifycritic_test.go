package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// criticFixture builds a serveCritic over a scripted provider and a tool
// registry that holds a write tool beside a read-only one. The provider's first
// turn calls the write tool, which is what a critic that tries to fix the code
// it was asked to judge would do.
func criticFixture(t *testing.T, wrote *atomic.Bool) (*serveCritic, *replay.Provider) {
	t.Helper()
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: []replay.Turn{
			{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.ToolCallBlock{ID: "w1", Name: "write_thing", Arguments: json.RawMessage(`{}`)},
				}},
				StopReason: llm.StopToolUse,
			},
			{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Text: "I could not change anything.\nMANVI-VERDICT[1]: PASS\n"},
				}},
				StopReason: llm.StopEndTurn,
			},
		},
	})
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(bus.New())
	for _, tool := range []tools.Tool{
		{
			Schema:   llm.ToolSchema{Name: "read_thing", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Handler:  func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "{}"} },
			ReadOnly: true,
		},
		{
			Schema: llm.ToolSchema{Name: "write_thing", InputSchema: json.RawMessage(`{"type":"object"}`)},
			Handler: func(context.Context, tools.Call) tools.Result {
				wrote.Store(true)
				return tools.Result{Text: "written"}
			},
		},
	} {
		if err := registry.Register(tool); err != nil {
			t.Fatal(err)
		}
	}
	runner := &subAgentRunner{}
	runner.attach(subAgentConfig{
		provider: provider, models: models, model: "m",
		registry: registry, systemPrompt: "parent", meter: &subAgentMeter{},
	})
	def, ok := agents.NewRegistry().Get(criticRole)
	if !ok {
		t.Fatal("no built-in critic role")
	}
	return &serveCritic{build: func(context.Context) (*criticRuntime, error) {
		return &criticRuntime{runner: runner, def: def}, nil
	}}, provider
}

// The critic is read-only by absence. The registry the child runs with has no
// write tool, so asking for one is asking for a tool that does not exist — not
// asking for one that refuses.
func TestServeCriticHasNoWriteToolsAndAWriteAttemptFindsNone(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixture(t, &wrote)

	summary, err := critic.Judge(context.Background(), "judge the change", 1)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if wrote.Load() {
		t.Fatal("the critic's write tool ran")
	}
	if !strings.Contains(summary, "MANVI-VERDICT[1]: PASS") {
		t.Fatalf("summary = %q", summary)
	}

	requests := provider.Requests()
	if len(requests) != 2 {
		t.Fatalf("the provider saw %d requests, want 2", len(requests))
	}
	offered := map[string]bool{}
	for _, schema := range requests[0].Tools {
		offered[schema.Name] = true
	}
	if offered["write_thing"] {
		t.Fatalf("the critic was offered the write tool: %v", offered)
	}
	if !offered["read_thing"] {
		t.Fatalf("the critic was not offered the read-only tool: %v", offered)
	}
	// The write attempt came back as an error, not as a result.
	var refused bool
	for _, message := range requests[1].Messages {
		for _, block := range message.Content {
			if result, ok := block.(llm.ToolResultBlock); ok && result.ToolCallID == "w1" && result.IsError {
				refused = true
			}
		}
	}
	if !refused {
		t.Fatal("the write attempt was not refused")
	}
}

// The judging role's own prompt carries the per-criterion contract, so the
// reader and the instruction are the same package's two halves.
func TestServeCriticIsToldThePerCriterionVerdictContract(t *testing.T) {
	var wrote atomic.Bool
	critic, provider := criticFixture(t, &wrote)
	if _, err := critic.Judge(context.Background(), "judge", 3); err != nil {
		t.Fatal(err)
	}
	system := provider.Requests()[0].System
	for _, want := range []string{"MANVI-VERDICT[n]: PASS", "3 numbered acceptance criteria", "adversarial"} {
		if !strings.Contains(strings.ToLower(system), strings.ToLower(want)) {
			t.Errorf("system prompt lacks %q:\n%s", want, system)
		}
	}
}

// A build failure is not remembered: the provider that was unreachable once may
// be reachable next time, and a cached error would degrade every verification
// until the host restarted.
func TestServeCriticDoesNotCacheABuildFailure(t *testing.T) {
	var wrote atomic.Bool
	good, _ := criticFixture(t, &wrote)
	calls := 0
	critic := &serveCritic{build: func(ctx context.Context) (*criticRuntime, error) {
		calls++
		if calls == 1 {
			return nil, context.DeadlineExceeded
		}
		return good.build(ctx)
	}}
	if _, err := critic.Judge(context.Background(), "judge", 1); err == nil {
		t.Fatal("the first build failure was swallowed")
	}
	if _, err := critic.Judge(context.Background(), "judge", 1); err != nil {
		t.Fatalf("the failure was cached: %v", err)
	}
}
