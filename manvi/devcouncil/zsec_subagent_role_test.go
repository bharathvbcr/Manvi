package devcouncil

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// spawn_subagents and invoke_subagent resolve a role name through one function.
// An unregistered name used to fail closed only in invoke: spawn ran it as a
// write-capable child on the parent's surface and reported nothing.
func TestSpawnWithAnUnknownRoleDoesNotProduceAWritingChild(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	payload := f.payload("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "reviewer", "prompt": "review it", "type": "critc"},
		},
	})

	runner.mu.Lock()
	seen := append([]SubAgentRequest(nil), runner.seen...)
	runner.mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("the runner was asked for %d children, want 1", len(seen))
	}
	if !seen[0].ReadOnly {
		t.Fatal("a misspelled role produced a child that can mutate the working tree")
	}
	unknown, _ := payload["unknown_types"].([]any)
	if len(unknown) != 1 || unknown[0] != "critc" {
		t.Fatalf("unknown_types = %v, want [critc]; payload %v", unknown, payload)
	}
	results, _ := payload["results"].([]any)
	outcome, _ := results[0].(map[string]any)
	if flagged, _ := outcome["unknown_type"].(bool); !flagged {
		t.Errorf("the child's own outcome does not say its role was unregistered: %v", outcome)
	}
}

// A task that names no role keeps the placement spawn has always given it, and
// is not reported as a typo.
func TestSpawnWithNoRoleIsNotReportedUnknown(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)
	payload := f.payload("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{{"label": "plain", "prompt": "do it"}},
	})
	if _, present := payload["unknown_types"]; present {
		t.Fatalf("a task naming no role was reported as an unknown type: %v", payload)
	}
}

// invoke has no "no role" reading: an empty type_name is a miss like any other
// and must not become a writing child.
func TestInvokeWithAnEmptyRoleNameIsReadOnly(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)
	payload := f.payload("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{{"role": "reviewer", "prompt": "review it"}},
	})
	runner.mu.Lock()
	seen := append([]SubAgentRequest(nil), runner.seen...)
	runner.mu.Unlock()
	if len(seen) != 1 || !seen[0].ReadOnly {
		t.Fatalf("an unnamed role produced a writable child: %v", seen)
	}
	if unknown, _ := payload["unknown_types"].([]any); len(unknown) != 1 {
		t.Fatalf("empty type_name not reported: %v", payload)
	}
}

// resolveRole is a floor on read-only, never a ceiling, for a registered role.
func TestResolveRoleReadOnlyFloorAndWriteExceptions(t *testing.T) {
	roles := agents.NewRegistry()
	if err := roles.Register(agents.Definition{
		Name: "builder", Role: "builds", SystemPrompt: "build", EnableWriteTools: true,
	}); err != nil {
		t.Fatal(err)
	}
	if got := resolveRole(roles, "builder", "x", false, false); !got.Known || got.ReadOnly {
		t.Fatalf("a writing role resolved read-only without being asked: %+v", got)
	}
	if got := resolveRole(roles, "builder", "x", true, false); !got.ReadOnly || got.Surface.Writes != nil {
		t.Fatalf("caller read_only did not floor the role: %+v", got)
	}
	if got := resolveRole(roles, "nope", "x", false, false); !got.Unknown || !got.ReadOnly || got.Known {
		t.Fatalf("unregistered role did not fail closed: %+v", got)
	}
}
