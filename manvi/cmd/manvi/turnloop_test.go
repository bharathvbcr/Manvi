package main

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/core/bus"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/replay"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// These pin what each of the three faces hands the agent loop, observed from
// outside the loop rather than read off the source: the output bound that
// reaches the wire, and whether the end-of-turn check is listening on the bus
// the loop actually dispatches on.
//
// The check is probed with a truncated first answer. The sensor's first rule
// is that a cut-off answer is not a finished one, so with the sensor attached
// the loop bounces and the model is asked a second time; without it the turn
// ends on the first reply. A sensor registered on the wrong bus is
// indistinguishable from no sensor, which is the drift this guards.

// truncatedThenDone is a model whose first answer hits the output cap and whose
// second finishes.
func truncatedThenDone() *replay.Provider {
	return replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 16384, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: []replay.Turn{
			{
				Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "half an ans"}},
				Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "half an ans"}}},
				StopReason: llm.StopMaxTokens,
			},
			{
				Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "the whole answer"}},
				Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "the whole answer"}}},
				StopReason: llm.StopEndTurn,
			},
		},
	})
}

// tuiTurn runs one TUI submission against p with the given flag overrides and
// returns what the provider was asked.
func tuiTurn(t *testing.T, p *replay.Provider, set map[string]string) []llm.Request {
	t.Helper()
	// The sensor captures a baseline of the working tree, so the turn runs in
	// a repository of its own, with the native surface production builds.
	t.Chdir(e2eRepo(t))
	t.Setenv("MANVI_HARNESS_INIT_ENABLED", "false")
	reg := registryWith(t, set)
	native, pipeline, err := nativeToolsWith(reg, nil)
	if err != nil {
		t.Fatalf("the native tool surface could not be built: %v", err)
	}
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	s := &tuiSession{
		id: "S1", storeID: "0123456789abcdef0123456789abcdef",
		log:      newSessionLog(),
		native:   native,
		pipeline: pipeline,
		subMeter: &subAgentMeter{},
		provider: p, registry: models, model: "m",
	}
	h := &harnessHost{reg: reg, sessions: map[string]*tuiSession{"S1": s}}
	if err := h.Submit(context.Background(), "S1", "answer me"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	return p.Requests()
}

// TestTheTUIBoundsOutputAtItsDefault: the TUI names an output bound of its own
// rather than leaving it to the adapter.
func TestTheTUIBoundsOutputAtItsDefault(t *testing.T) {
	reqs := tuiTurn(t, truncatedThenDone(), nil)
	if len(reqs) == 0 {
		t.Fatal("the model was never asked")
	}
	if got := reqs[0].MaxTokens; got != 8192 {
		t.Fatalf("TUI request MaxTokens = %d, want the face's default 8192", got)
	}
}

// TestTheTUIBoundsOutputAtTheLocalCap: with the local provider selected, the
// operator's local output cap is the bound.
func TestTheTUIBoundsOutputAtTheLocalCap(t *testing.T) {
	reqs := tuiTurn(t, truncatedThenDone(), map[string]string{
		"llm.provider.default":        "local",
		"llm.local.max_output_tokens": "1234",
	})
	if len(reqs) == 0 {
		t.Fatal("the model was never asked")
	}
	if got := reqs[0].MaxTokens; got != 1234 {
		t.Fatalf("TUI request MaxTokens = %d, want the local cap 1234", got)
	}
}

// TestTheTUIsEndOfTurnCheckIsOnTheLoopsBus: a truncated answer is bounced.
func TestTheTUIsEndOfTurnCheckIsOnTheLoopsBus(t *testing.T) {
	reqs := tuiTurn(t, truncatedThenDone(), nil)
	if len(reqs) != 2 {
		t.Fatalf("the model was asked %d time(s), want 2: a truncated answer the sensor "+
			"did not bounce means the check is not listening on the loop's bus", len(reqs))
	}
}

// TestASubAgentLeavesTheOutputBoundToTheAdapterAndHasNoSensor: a child names no
// output bound, and has no end-of-turn check of its own — its written paths
// come back to the parent, whose check covers them (SubAgentResult.Wrote).
func TestASubAgentLeavesTheOutputBoundToTheAdapterAndHasNoSensor(t *testing.T) {
	p := truncatedThenDone()
	models := llm.NewRegistry()
	if err := models.Register(p); err != nil {
		t.Fatal(err)
	}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: p, models: models, model: "m",
		registry: tools.NewRegistry(bus.New()), systemPrompt: "child prompt",
	})
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "worker", Prompt: "answer me", ReadOnly: true,
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}
	reqs := p.Requests()
	if len(reqs) != 1 {
		t.Fatalf("the child was asked %d time(s), want 1: a child has no end-of-turn check, "+
			"so nothing bounces its truncated answer", len(reqs))
	}
	if got := reqs[0].MaxTokens; got != 0 {
		t.Fatalf("child request MaxTokens = %d, want 0 (left to the adapter)", got)
	}
}

// TestHeadlessRunLeavesTheOutputBoundToTheAdapterAndChecksTheTurn drives the
// shipped binary. `manvi run` names no output bound — the local adapter then
// applies the window it discovered, which is the point of leaving it zero —
// and its end-of-turn check bounces a truncated answer like the TUI's does.
func TestHeadlessRunLeavesTheOutputBoundToTheAdapterAndChecksTheTurn(t *testing.T) {
	model := &e2eModel{reply: func(turn int) (map[string]any, string) {
		if turn == 1 {
			return map[string]any{"content": "half an ans"}, "length"
		}
		return map[string]any{"content": "the whole answer"}, "stop"
	}}
	base := model.start(t)
	repo := e2eRepo(t)

	state := t.TempDir()
	out, status := e2eRun(t, repo, base, "answer me", "MANVI_STATE_DIR="+state)
	if status != 0 {
		t.Fatalf("exit %d\n%s", status, out)
	}
	seen := model.seen()
	if len(seen) != 2 {
		t.Fatalf("the model server saw %d request(s), want 2: a truncated answer the sensor "+
			"did not bounce means the check is not listening on the loop's bus\n%s", len(seen), out)
	}
	// Not the TUI's 8192: the bound is the adapter's, from discovery.
	if got := seen[0].MaxTokens; got == 8192 || got <= 0 {
		t.Fatalf("run sent max_tokens=%d, want the adapter's discovered bound", got)
	}

	// The session store is the durable record of the turn; the check's report
	// is in it whatever the face printed.
	var verified bool
	walkErr := filepath.WalkDir(state, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		// #nosec G304 -- a path under this test's own t.TempDir.
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), "verify/report") {
			verified = true
		}
		return err
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if !verified {
		t.Fatal("the saved session records no end-of-turn check")
	}
}
