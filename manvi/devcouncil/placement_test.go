package devcouncil

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// fixtureWithRoles builds the tool surface with a role catalogue attached, so
// a dispatch has definitions to read. newFixtureRunner leaves SubagentRegistry
// nil, which makes getSubagentRegistry hand back a throwaway — fine for the
// untyped fan-out, useless for asserting a role reached the runner.
func fixtureWithRoles(t *testing.T, runner SubAgentRunner, defs ...agents.Definition) (*fixture, *agents.Registry) {
	t.Helper()
	roles := agents.NewRegistry()
	for _, d := range defs {
		if err := roles.Register(d); err != nil {
			t.Fatalf("registering role %q: %v", d.Name, err)
		}
	}
	f := newFixtureRunner(t, runner)
	f.reg.deps.SubagentRegistry = roles
	f.reg.deps.SubagentMgr = agents.NewInstanceManager()
	return f, roles
}

// A role's Model and SystemPrompt must reach the runner. This is the whole of
// the multi-model ask: a planner on a frontier model dispatching workers to a
// local one is only expressible if the placement survives the dispatch.
func TestInvokeSubagentCarriesTheRolePlacement(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:             "local_worker",
		Role:             "Local Worker",
		Model:            "local/qwen3-27b-mlx",
		SystemPrompt:     "You are a local worker.",
		EnableWriteTools: true,
	})

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "local_worker", "role": "worker-1", "prompt": "do the work"},
		},
	})
	if res.IsError {
		t.Fatalf("invoke_subagent failed: %s", res.Text)
	}

	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}
	got := runner.seen[0]
	if got.ModelSpec != "local/qwen3-27b-mlx" {
		t.Errorf("ModelSpec = %q, want the role's model to have survived dispatch", got.ModelSpec)
	}
	if got.SystemPrompt != "You are a local worker." {
		t.Errorf("SystemPrompt = %q, want the role's prompt to have survived dispatch", got.SystemPrompt)
	}
	if got.ReadOnly {
		t.Errorf("ReadOnly = true, want a writing child for a role with EnableWriteTools")
	}
}

// The invoke schema has advertised a per-call `model` since it shipped. It was
// once honoured as any placement at all, which let the dispatching model put a
// child on whatever id it knew; it now names a tier within the role's ceiling,
// and that tier replaces the router's choice.
func TestInvokeSubagentPerCallTierOverridesTheRouting(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "research", "role": "r", "prompt": "survey it", "model": "basic"},
		},
	})
	if res.IsError {
		t.Fatalf("invoke_subagent failed: %s", res.Text)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}
	if got := runner.seen[0].Tier; got != agents.TierBasic {
		t.Fatalf("Tier = %q, want the per-call tier to replace the routed lite", got)
	}
}

// A role the operator pinned to a model stays there: a per-call tier is
// refused rather than silently ignored or allowed to move it.
func TestInvokeSubagentPerCallTierCannotMoveAnOperatorPinnedRole(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:  "local_reader",
		Role:  "Local Reader",
		Model: "local/qwen3-27b-mlx",
	})

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "local_reader", "role": "p", "prompt": "read it", "model": "lite"},
		},
	})
	if !res.IsError {
		t.Fatalf("a per-call tier moved an operator-pinned role: %s", res.Text)
	}
	if !strings.Contains(res.Text, "local/qwen3-27b-mlx") {
		t.Errorf("the refusal does not name the operator's placement: %s", res.Text)
	}
	if len(runner.seen) != 0 {
		t.Fatalf("the runner was asked for %d children after a refusal", len(runner.seen))
	}
}

// An unknown type falls back to a synthetic definition. It must inherit rather
// than pick up a stale placement from whatever role was asked for.
func TestInvokeSubagentUnknownTypeInherits(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "no_such_role", "role": "r", "prompt": "work"},
		},
	})
	if res.IsError {
		t.Fatalf("invoke_subagent failed: %s", res.Text)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}
	if got := runner.seen[0]; !agents.ParsePlacement(got.ModelSpec, nil).Inherits() {
		t.Fatalf("ModelSpec = %q, want an unknown type to inherit the parent placement", got.ModelSpec)
	}
}

// The untyped fan-out may name a role too, which is what lets one call put a
// planner on one model and its workers on another.
func TestSpawnSubagentsHonoursAPerTaskType(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:             "worker",
		Role:             "Worker",
		Model:            "local/qwen3-27b-mlx",
		SystemPrompt:     "You are a worker.",
		EnableWriteTools: true,
	})

	res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "typed", "prompt": "do it", "type": "worker"},
			{"label": "untyped", "prompt": "do it too"},
		},
	})
	if res.IsError {
		t.Fatalf("spawn_subagents failed: %s", res.Text)
	}

	seen := map[string]SubAgentRequest{}
	for _, req := range runner.seen {
		seen[req.Label] = req
	}
	if got := seen["typed"].ModelSpec; got != "local/qwen3-27b-mlx" {
		t.Errorf("typed task ModelSpec = %q, want the role's model", got)
	}
	if got := seen["typed"].SystemPrompt; got != "You are a worker." {
		t.Errorf("typed task SystemPrompt = %q, want the role's prompt", got)
	}
	// A task that names no type must be unaffected: the fan-out that existed
	// before roles could be named still runs on the parent's placement.
	if got := seen["untyped"]; got.ModelSpec != "" || got.SystemPrompt != "" {
		t.Errorf("untyped task = %+v, want it to inherit the parent placement", got)
	}
}
