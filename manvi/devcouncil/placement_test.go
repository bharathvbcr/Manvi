package devcouncil

import (
	"testing"

	"manvi/agents"
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

// The invoke schema has advertised a per-call `model` since it shipped, and the
// handler decoded it into a field it never read. A schema field that is parsed
// and dropped tells the model it has control it does not have.
func TestInvokeSubagentPerCallModelOverridesTheRole(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name:  "planner",
		Role:  "Planner",
		Model: "anthropic/claude-opus-4-5",
	})

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "planner", "role": "p", "prompt": "plan it", "model": "local"},
		},
	})
	if res.IsError {
		t.Fatalf("invoke_subagent failed: %s", res.Text)
	}
	if len(runner.seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(runner.seen))
	}
	if got := runner.seen[0].ModelSpec; got != "local" {
		t.Fatalf("ModelSpec = %q, want the per-call override %q to win over the role", got, "local")
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
