package devcouncil

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/session"
)

// runWriteThroughLoop drives the real agent loop over the real tool registry
// for one write call and returns the outcome and what the checkpoint was told.
func runWriteThroughLoop(t *testing.T, f *fixture, path string) (agent.Outcome, agent.TurnStopping) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": path, "content": "x\n"})
	if err != nil {
		t.Fatal(err)
	}
	call := llm.ToolCallBlock{ID: "call-1", Name: "devcouncil_write_file", Arguments: args}
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Model: "test-model", Provider: "replay", ContextWindow: 100000, MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: []replay.Turn{
			{
				Chunks:     []llm.Chunk{{Kind: llm.ChunkToolCallStart, ToolCallID: "call-1", ToolName: call.Name}},
				Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{call}},
				StopReason: llm.StopToolUse,
				Usage:      llm.Usage{InputTokens: 20, OutputTokens: 8},
			},
			{
				Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
				Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}},
				StopReason: llm.StopEndTurn,
				Usage:      llm.Usage{InputTokens: 10, OutputTokens: 5},
			},
		},
	})
	b := bus.New()
	loop, err := agent.NewLoop(agent.Config{
		Provider: provider, Model: "test-model", SystemPrompt: "builder",
		MaxSteps: 8, MaxTokens: 1024,
	}, b, session.NewLog(), f.pipe)
	if err != nil {
		t.Fatal(err)
	}
	var stopping agent.TurnStopping
	fired := false
	if _, err := bus.OnSerial(b, func(_ context.Context, e *agent.TurnStopping) error {
		stopping, fired = *e, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	out, err := loop.Run(context.Background(), llm.Message{
		Role: llm.RoleUser, Content: []llm.ContentBlock{llm.TextBlock{Text: "write it"}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !fired {
		t.Fatal("TurnStopping never fired")
	}
	return out, stopping
}

// TestWriteCarryingAPolicyRuleStillCountsAsMutation drives the real registry:
// a write that policy allowed but qualified (a protected-file warning under a
// lease, or an unleased write under yolo) must reach the end-of-turn check as a
// mutation, or that check is skipped for exactly the writes policy says need it.
func TestWriteCarryingAPolicyRuleStillCountsAsMutation(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]string
		lease    bool
		path     string
	}{
		{"leased protected file, default enforce", map[string]string{}, true, "package.json"},
		{"yolo, no task", map[string]string{flags.HarnessPosture: flags.PostureYolo}, false, "a.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixtureWith(t, tc.settings)
			if tc.lease {
				checkout(t, f)
			}
			out, stopping := runWriteThroughLoop(t, f, tc.path)
			if len(out.Wrote) != 1 || out.Wrote[0] != tc.path {
				t.Fatalf("Wrote = %v, want [%s]; the write did not land: %+v", out.Wrote, tc.path, out)
			}
			if !out.Mutated || !stopping.Mutated {
				t.Fatalf("Outcome.Mutated=%v TurnStopping.Mutated=%v, want both true", out.Mutated, stopping.Mutated)
			}
		})
	}
}
