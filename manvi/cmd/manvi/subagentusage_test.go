package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"manvi/core/bus"
	"manvi/devcouncil"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/tools"
)

// usageFixture is a two-step child: one tool call, then an answer. The token
// counts are distinct primes so a total that drops or double-counts a step is
// visible in the number rather than only in a boolean.
func usageFixture() *replay.Provider {
	return replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: []replay.Turn{
			{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.ToolCallBlock{ID: "c1", Name: "noop", Arguments: json.RawMessage(`{}`)},
				}},
				StopReason: llm.StopToolUse,
				Usage:      llm.Usage{InputTokens: 101, OutputTokens: 11, ReasoningTokens: 3, CacheReadTokens: 50},
			},
			{
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.TextBlock{Text: "done"},
				}},
				StopReason: llm.StopEndTurn,
				Usage:      llm.Usage{InputTokens: 211, OutputTokens: 13, ReasoningTokens: 5, CacheReadTokens: 60},
			},
		},
	})
}

func usageRunner(t *testing.T, meter *subAgentMeter) *subAgentRunner {
	t.Helper()
	provider := usageFixture()
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry(bus.New())
	if err := registry.Register(tools.Tool{
		Schema:  llm.ToolSchema{Name: "noop", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(context.Context, tools.Call) tools.Result { return tools.Result{Text: "{}"} },
	}); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: provider, models: models, model: "m",
		registry: registry, systemPrompt: "child", meter: meter,
	})
	return r
}

// TestASubAgentReportsWhatItSpent.
//
// SubAgentResult carried a summary and a step count and nothing about cost, so
// the runner read outcome.Usage and dropped it. The run's usage line therefore
// counted the dispatching agent alone — measured on an eight-way fan-out
// against a scripted server, 2,200 of 38,200 input tokens, 5.8% of the real
// spend, reported as the whole of it. A benchmark records that number.
func TestASubAgentReportsWhatItSpent(t *testing.T) {
	meter := &subAgentMeter{}
	r := usageRunner(t, meter)

	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "worker", Prompt: "do the thing",
	})
	if err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	want := devcouncil.SubAgentUsage{
		InputTokens: 312, OutputTokens: 24, ReasoningTokens: 8, CacheReadTokens: 110,
	}
	if out.Usage != want {
		t.Errorf("the child reported %+v, want %+v — a fan-out's cost is invisible without this",
			out.Usage, want)
	}
	if got := meter.Total(); got != want {
		t.Errorf("the run meter holds %+v, want %+v", got, want)
	}
}

// TestTheRunMeterSumsEveryChildIncludingConcurrentOnes.
//
// The meter is written from every child's goroutine at once, so it is both a
// correctness assertion and a race assertion; it is meaningful under -race and
// harmless without it.
func TestTheRunMeterSumsEveryChildIncludingConcurrentOnes(t *testing.T) {
	const children = 8
	meter := &subAgentMeter{}

	var wg sync.WaitGroup
	for i := 0; i < children; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A runner each, because a replay provider plays one script.
			r := usageRunner(t, meter)
			if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
				Label: "worker", Prompt: "do the thing",
			}); err != nil {
				t.Errorf("child %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got := meter.Total()
	if got.InputTokens != 312*children || got.OutputTokens != 24*children {
		t.Errorf("meter = %+v, want %d in / %d out — a fan-out's children are where the spend is",
			got, 312*children, 24*children)
	}
}

// TestAFailedChildStillReportsWhatItSpent.
//
// A child that ran and produced nothing did the work of running. Counting only
// the children that succeeded under-reports exactly the turns an operator is
// investigating, and makes a failing fan-out look cheap.
func TestAFailedChildStillReportsWhatItSpent(t *testing.T) {
	meter := &subAgentMeter{}
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		// One turn with no text and no tool call: the child finishes with
		// nothing to say, which the dispatch treats as a failure.
		Turns: []replay.Turn{{
			Message:    llm.Message{Role: llm.RoleAssistant},
			StopReason: llm.StopEndTurn,
			Usage:      llm.Usage{InputTokens: 401, OutputTokens: 7},
		}},
	})
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: provider, models: models, model: "m",
		registry: tools.NewRegistry(bus.New()), systemPrompt: "child", meter: meter,
	})

	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "worker", Prompt: "do the thing",
	})
	if err == nil {
		t.Fatal("a child that produced no answer was reported as a success")
	}
	if out.Usage.InputTokens != 401 {
		t.Errorf("a failed child reported %d input tokens, want 401 — it spent them either way",
			out.Usage.InputTokens)
	}
	if got := meter.Total().InputTokens; got != 401 {
		t.Errorf("the run meter holds %d input tokens from a failed child, want 401", got)
	}
}
