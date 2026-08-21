package main

import (
	"context"
	"encoding/json"
	"fmt"
	"manvi/core/bus"
	"manvi/llm"
	"manvi/llm/replay"
	"manvi/ui"
	"strings"
	"sync"
	"testing"

	"manvi/devcouncil"
	"manvi/flags"
	"manvi/tools"
)

// An unattached runner refuses. It must never invent a result.
//
// The tool this serves once returned `"status":"completed"` with a summary
// composed from the prompt, for a child that had never run. A dispatching agent
// reading four of those believes it received four analyses.
func TestAnUnattachedSubAgentRunnerRefuses(t *testing.T) {
	var r subAgentRunner
	out, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label:  "analysis",
		Prompt: "review the parser",
	})
	if err == nil {
		t.Fatalf("an unattached runner reported success: %+v", out)
	}
	if out.Summary != "" {
		t.Errorf("an unattached runner produced a summary: %q", out.Summary)
	}
	if !strings.Contains(err.Error(), "no model is attached") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// An empty prompt is refused before a lease or a turn is spent on it.
func TestASubAgentWithNoPromptIsRefused(t *testing.T) {
	r := &subAgentRunner{}
	r.attach(subAgentConfig{model: "m"})
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{Label: "x"}); err == nil {
		t.Fatal("a sub-agent with no prompt was accepted")
	}
}

// The two safety properties of a child's tool surface, asserted against the
// real registry rather than a fixture: a child can never spawn a child, and a
// read-only child cannot reach a mutating tool even by naming it.
func TestAChildToolSurfaceCannotRecurseOrWrite(t *testing.T) {
	reg, err := flags.NewHarnessRegistry("")
	if err != nil {
		t.Fatal(err)
	}
	_, pipeline, err := nativeTools(reg)
	if err != nil {
		t.Fatal(err)
	}
	if !pipeline.Has(spawnSubagentsTool) {
		t.Fatalf("%s is not registered; this test no longer proves anything", spawnSubagentsTool)
	}

	writable := pipeline.Subset(func(tool tools.Tool) bool {
		return tool.Schema.Name != spawnSubagentsTool
	})
	if writable.Has(spawnSubagentsTool) {
		t.Error("a child was given the tool that spawns children; depth is unbounded")
	}
	if !writable.Has("devcouncil_write_file") {
		t.Error("a writable child lost its write tools")
	}

	readOnly := pipeline.Subset(func(tool tools.Tool) bool {
		return tool.Schema.Name != spawnSubagentsTool && tool.ReadOnly
	})
	if readOnly.Has(spawnSubagentsTool) {
		t.Error("a read-only child was given the tool that spawns children")
	}
	for _, mutating := range []string{
		"devcouncil_write_file", "devcouncil_patch_file",
		"devcouncil_delete_file", "devcouncil_exec_command",
	} {
		if !pipeline.Has(mutating) {
			continue // not registered in this build; nothing to assert
		}
		if readOnly.Has(mutating) {
			t.Errorf("a read-only child can reach %s", mutating)
		}
		res := readOnly.Run(context.Background(), tools.Call{
			ID: "1", Name: mutating, Arguments: []byte(`{}`),
		})
		if !res.IsError || !strings.Contains(res.Text, "unknown tool") {
			t.Errorf("a read-only child ran %s: err=%v %q", mutating, res.IsError, res.Text)
		}
	}
}

// TestASubAgentsEvidenceReachesTheOperator.
//
// A child keeps its own log — sharing the parent's would interleave two
// conversations into one projection, and the projection is what the next
// request is built from. But nothing ever observed that log, so everything in
// it was garbage-collected when the function returned: every gate refusal,
// every grant, every adapter compensation. A four-way fan-out whose every write
// the gate refused left no record anywhere, and the parent consumed the child's
// own prose as the only account of what happened.
//
// Structural, in the same style as the effort-ceiling and shared-notices tests:
// the defect is a wiring gap, and it is visible in the source.
func TestASubAgentsEvidenceReachesTheOperator(t *testing.T) {
	src, err := readSource("cmd/manvi/subagent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "log.Observe(") {
		t.Fatal("the child's log is never observed, so every policy decision, grant and " +
			"compensation it records is discarded when the runner returns")
	}
	if !strings.Contains(src, "out.Agent = label") {
		t.Error("child events are forwarded unattributed; two agents' evidence in one stream " +
			"is only readable if each line says whose it is")
	}
	// The child's own log must still be its own — forwarding is not sharing.
	if !strings.Contains(src, "log := session.NewLog()") {
		t.Error("the child no longer has its own log; sharing the parent's would put the child's " +
			"tool results into the history the parent replays to the model")
	}

	// Both faces have to supply the sink, or the field is a decoration that
	// defaults to nil and forwards nothing.
	for _, f := range []string{"cmd/manvi/run.go", "cmd/manvi/tui.go"} {
		caller, err := readSource(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(caller, "sink:") {
			t.Errorf("%s attaches the sub-agent runner without a sink, so children run there "+
				"with their evidence still going nowhere", f)
		}
	}
}

// capturingSink records everything a face would have been shown.
type capturingSink struct {
	mu     sync.Mutex
	events []ui.Event
}

func (s *capturingSink) Emit(e ui.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
}

// TestAChildsGateRefusalReachesTheSink drives a real child turn and asserts the
// evidence survives it.
//
// This is the behavioural half of TestASubAgentsEvidenceReachesTheOperator. A
// child keeps its own log, and nothing observed it, so a gate refusal inside a
// fan-out was recorded into a log that was garbage-collected when the runner
// returned. The parent's counters only ever counted the parent's own calls, so
// a four-way fan-out whose every write was refused summarised as a clean run.
func TestAChildsGateRefusalReachesTheSink(t *testing.T) {
	const label = "worker-1"

	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: []replay.Turn{
			{
				Chunks: []llm.Chunk{{Kind: llm.ChunkToolCallStart, ToolCallID: "c1", ToolName: "write"}},
				Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
					llm.ToolCallBlock{ID: "c1", Name: "write", Arguments: json.RawMessage(`{"path":"x.go"}`)},
				}},
				StopReason: llm.StopToolUse,
			},
			{
				Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "could not write"}},
				Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "could not write"}}},
				StopReason: llm.StopEndTurn,
			},
		},
	})
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}

	registry := tools.NewRegistry(bus.New())
	if err := registry.Register(tools.Tool{
		Schema: llm.ToolSchema{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(context.Context, tools.Call) tools.Result {
			// Exactly what the gate returns for a refused write.
			return tools.Result{
				Text: `{"rule":"scope.unplanned","allowed":false}`, IsError: true,
				Blocked: true, Rule: "scope.unplanned", Severity: "soft",
			}
		},
	}); err != nil {
		t.Fatal(err)
	}

	sink := &capturingSink{}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: provider, models: models, model: "m",
		registry: registry, systemPrompt: "child prompt", sink: sink,
	})

	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: label, Prompt: "write x.go",
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	var sawRefusal bool
	for _, e := range sink.events {
		if e.Agent != label {
			continue
		}
		if e.Rule == "scope.unplanned" {
			sawRefusal = true
		}
	}
	if !sawRefusal {
		t.Fatalf("the child's gate refusal never reached the sink; %d event(s) captured, "+
			"none attributed to %q carrying the rule. A fan-out whose every write was refused "+
			"would summarise as a clean run", len(sink.events), label)
	}

	// And the child's prose must not be interleaved into the parent's
	// transcript — its answer already returns through the tool result.
	for _, e := range sink.events {
		if e.Agent == label && (e.Kind == ui.KindText || e.Kind == ui.KindReasoning) {
			t.Errorf("a child assistant chunk was forwarded: %q", e.Text)
		}
	}
}

// TestParallelChildrenShareTheSinkSafely.
//
// A fan-out runs its children concurrently through agents.Pool, and every child
// now forwards its evidence into one sink shared with the parent. Before that
// change the sink only ever saw sequential emissions from the parent's own
// loop; it now sees concurrent ones from N child goroutines.
//
// The three real sinks are safe by construction — Renderer and JSONSink each
// take a mutex, and the TUI's is a channel send — but "safe by construction"
// is a reading, and this is the kind of hazard that passes a reading and fails
// in production. Run with -race.
func TestParallelChildrenShareTheSinkSafely(t *testing.T) {
	const children = 8

	newProvider := func() *replay.Provider {
		return replay.New(replay.Fixture{
			Provider: "replay",
			Capabilities: []llm.Capability{{
				Provider: "replay", Model: "m", ContextWindow: 32768,
				MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
			}},
			Turns: []replay.Turn{
				{
					Chunks: []llm.Chunk{{Kind: llm.ChunkToolCallStart, ToolCallID: "c1", ToolName: "write"}},
					Message: llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
						llm.ToolCallBlock{ID: "c1", Name: "write", Arguments: json.RawMessage(`{"path":"x.go"}`)},
					}},
					StopReason: llm.StopToolUse,
				},
				{
					Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "refused"}},
					Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "refused"}}},
					StopReason: llm.StopEndTurn,
				},
			},
		})
	}

	sink := &capturingSink{}
	var wg sync.WaitGroup
	for i := 0; i < children; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			// Each child gets its own provider and registry, exactly as the
			// runner builds them per call; the sink is the one shared thing.
			provider := newProvider()
			models := llm.NewRegistry()
			if err := models.Register(provider); err != nil {
				t.Errorf("register: %v", err)
				return
			}
			registry := tools.NewRegistry(bus.New())
			if err := registry.Register(tools.Tool{
				Schema: llm.ToolSchema{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)},
				Handler: func(context.Context, tools.Call) tools.Result {
					return tools.Result{
						Text: `{"allowed":false}`, IsError: true,
						Blocked: true, Rule: "scope.unplanned", Severity: "soft",
					}
				},
			}); err != nil {
				t.Errorf("register tool: %v", err)
				return
			}
			r := &subAgentRunner{}
			r.attach(subAgentConfig{
				provider: provider, models: models, model: "m",
				registry: registry, systemPrompt: "child", sink: sink,
			})
			if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
				Label: fmt.Sprintf("worker-%d", n), Prompt: "write x.go",
			}); err != nil {
				t.Errorf("child %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	// Every child's refusal must be present and attributed to it. A lost event
	// is the failure this whole change exists to prevent; a mislabelled one is
	// worse, because it accuses the wrong child.
	seen := map[string]bool{}
	sink.mu.Lock()
	for _, e := range sink.events {
		if e.Rule == "scope.unplanned" && e.Agent != "" {
			seen[e.Agent] = true
		}
	}
	sink.mu.Unlock()
	for i := 0; i < children; i++ {
		if label := fmt.Sprintf("worker-%d", i); !seen[label] {
			t.Errorf("no refusal attributed to %s; %d distinct children reported", label, len(seen))
		}
	}
}

// TestAChildsTurnFrameIsNotForwarded.
//
// ui.Project maps a child's own bookkeeping onto this session's turn
// delimiters: session.UserMessage becomes KindTurnStart and session.TurnEnd
// becomes KindTurnEnd. Forwarding those put one turn.start and one turn.end per
// child inside a single parent turn — on a stream whose documented contract is
// that a consumer can delimit turns by kind — and rendered each child's prompt
// in the terminal with the same ▌ banner a user submission gets.
//
// A child's turn is not a turn of this session. Its evidence belongs in the
// transcript; its frame does not.
func TestAChildsTurnFrameIsNotForwarded(t *testing.T) {
	provider := replay.New(replay.Fixture{
		Provider: "replay",
		Capabilities: []llm.Capability{{
			Provider: "replay", Model: "m", ContextWindow: 32768,
			MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
		}},
		Turns: []replay.Turn{{
			Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
			Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}},
			StopReason: llm.StopEndTurn,
		}},
	})
	models := llm.NewRegistry()
	if err := models.Register(provider); err != nil {
		t.Fatal(err)
	}

	sink := &capturingSink{}
	r := &subAgentRunner{}
	r.attach(subAgentConfig{
		provider: provider, models: models, model: "m",
		registry: tools.NewRegistry(bus.New()), systemPrompt: "child", sink: sink,
	})
	if _, err := r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
		Label: "worker-1", Prompt: "secret child prompt",
	}); err != nil {
		t.Fatalf("RunSubAgent: %v", err)
	}

	sink.mu.Lock()
	defer sink.mu.Unlock()
	for _, e := range sink.events {
		switch e.Kind {
		case ui.KindTurnStart:
			t.Errorf("a child emitted turn.start into the parent's stream (text %q); a consumer "+
				"delimiting turns by kind sees this turn begin again", e.Text)
		case ui.KindTurnEnd:
			t.Error("a child emitted turn.end into the parent's stream; a consumer delimiting " +
				"turns by kind sees this turn end before it has")
		}
	}
}

// TestAttachDuringAFanOutIsNotARace.
//
// place() opened with an unsynchronised `cfg := r.cfg` while attach wrote that
// field under Lock. RunSubAgent already snapshots under RLock and nil-checks
// the snapshot; place threw it away and re-read. Two defects in one line — the
// race, and a child being placed against a different config than the one its
// registry, floor and nil-check were built from.
//
// Reachable: the TUI re-attaches on every submission, because provider, model
// and effort are switchable mid-session. Run with -race.
func TestAttachDuringAFanOutIsNotARace(t *testing.T) {
	newProvider := func() *replay.Provider {
		return replay.New(replay.Fixture{
			Provider: "replay",
			Capabilities: []llm.Capability{{
				Provider: "replay", Model: "m", ContextWindow: 32768,
				MaxOutputTokens: 4096, SupportsTools: true, SupportsStreaming: true,
			}},
			Turns: []replay.Turn{{
				Chunks:     []llm.Chunk{{Kind: llm.ChunkText, Text: "done"}},
				Message:    llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{llm.TextBlock{Text: "done"}}},
				StopReason: llm.StopEndTurn,
			}},
		})
	}
	mk := func() subAgentConfig {
		p := newProvider()
		models := llm.NewRegistry()
		if err := models.Register(p); err != nil {
			t.Fatal(err)
		}
		return subAgentConfig{
			provider: p, models: models, model: "m",
			registry: tools.NewRegistry(bus.New()), systemPrompt: "child",
		}
	}

	r := &subAgentRunner{}
	r.attach(mk())

	var wg sync.WaitGroup
	// Children running while the host re-attaches, which is the TUI's
	// every-submission behaviour.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _ = r.RunSubAgent(context.Background(), devcouncil.SubAgentRequest{
				Label: fmt.Sprintf("w-%d", n), Prompt: "do a thing",
			})
		}(i)
	}
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); r.attach(mk()) }()
	}
	wg.Wait()
}
