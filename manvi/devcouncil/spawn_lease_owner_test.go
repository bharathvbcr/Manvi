package devcouncil

import (
	"context"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// ownerProbeRunner reads the lease the dispatch holds on the child's task while
// the child runs, which is the moment the lease owner is what watch and lease
// faces print as the holder.
type ownerProbeRunner struct {
	fixture *fixture
	task    string
	owner   string
	seen    bool
}

func (r *ownerProbeRunner) RunSubAgent(ctx context.Context, _ SubAgentRequest) (SubAgentResult, error) {
	if l, err := r.fixture.reg.deps.Store.Active(ctx, r.task); err == nil && l != nil {
		r.owner, r.seen = l.Owner, true
	}
	return SubAgentResult{Summary: "done", Steps: 1}, nil
}

// A model chooses the label of a spawned sub-agent. Stored raw it became the
// lease owner, so "human:operator" read as the operator and an unbounded label
// went into the store; checkout already prefixes and bounds its owner, and
// spawn must produce the same thing.
func TestSpawnedChildLeaseOwnerIsAgentPrefixedAndBounded(t *testing.T) {
	for _, label := range []string{"human:operator", strings.Repeat("x", 500), "has space\nnewline"} {
		probe := &ownerProbeRunner{task: "TASK-001"}
		f, _ := fixtureWithRoles(t, probe, agents.Definition{Name: "writer", Role: "Writer", EnableWriteTools: true})
		probe.fixture = f
		res := f.call("devcouncil_spawn_subagents", map[string]any{
			"tasks": []map[string]any{{"label": label, "prompt": "work", "type": "writer", "task_id": "TASK-001"}},
		})
		if res.IsError {
			t.Fatalf("label %q: dispatch failed: %s", label, res.Text)
		}
		if !probe.seen {
			t.Fatalf("label %q: no lease was held while the child ran", label)
		}
		if !strings.HasPrefix(probe.owner, "agent:") {
			t.Errorf("label %q: lease owner %q does not start with agent:, so it can pose as an operator", label, probe.owner)
		}
		if want := len("agent:") + maxLeaseLabel; len(probe.owner) > want {
			t.Errorf("label %q: lease owner is %d bytes, bound is %d", label, len(probe.owner), want)
		}
		if strings.Count(probe.owner, ":") != 1 {
			t.Errorf("label %q: lease owner %q carries a colon of the model's choosing", label, probe.owner)
		}
	}
}
