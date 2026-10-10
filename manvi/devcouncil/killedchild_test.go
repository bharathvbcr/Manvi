package devcouncil

import (
	"context"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// killingRunner is terminated by the control plane while it works, then still
// returns a summary: the race that leaves completed-after-canceling refused.
type killingRunner struct{ mgr *agents.InstanceManager }

func (r *killingRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	if _, err := r.mgr.KillAll(); err != nil {
		return SubAgentResult{}, err
	}
	return SubAgentResult{Summary: "late findings for " + req.Label, Steps: 1}, nil
}

// A child killed mid-flight whose completion is then refused must end errored.
// It stayed canceling forever, which a later kill re-killed and reported as a
// success.
func TestChildKilledMidFlightEndsErroredNotCanceling(t *testing.T) {
	runner := &killingRunner{}
	f, _ := fixtureWithRoles(t, runner, agents.Definition{Name: "w", Role: "W", EnableWriteTools: true})
	runner.mgr = f.reg.deps.SubagentMgr
	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{{"type_name": "w", "role": "w-1", "prompt": "go"}},
	})
	_ = res
	snaps := f.reg.deps.SubagentMgr.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("want 1 instance, got %d", len(snaps))
	}
	if snaps[0].State != agents.StateErrored {
		t.Fatalf("state = %q after a refused completion, want %q", snaps[0].State, agents.StateErrored)
	}
}
