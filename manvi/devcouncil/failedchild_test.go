package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// partialRunner is a child that changed files and then did not complete: the
// runner returns what it wrote alongside the failure, exactly as the
// production runner does ("every exit returns this, so the paths travel even
// on the failure paths").
type partialRunner struct {
	wrote     []string
	truncated bool
	usage     SubAgentUsage
	err       error // nil: the child "finished" but returned no summary
}

func (r *partialRunner) RunSubAgent(context.Context, SubAgentRequest) (SubAgentResult, error) {
	return SubAgentResult{Wrote: r.wrote, WroteTruncated: r.truncated, Usage: r.usage, Steps: 2}, r.err
}

func dispatchBoth(t *testing.T, runner SubAgentRunner) map[string]tools.Result {
	t.Helper()
	f, _ := fixtureWithRoles(t, runner, agents.Definition{
		Name: "writer", Role: "Writer", EnableWriteTools: true,
	})
	return map[string]tools.Result{
		"invoke_subagent": f.call("devcouncil_invoke_subagent", map[string]any{
			"subagents": []map[string]any{{"type_name": "writer", "role": "w", "prompt": "change it"}},
		}),
		"spawn_subagents": f.call("devcouncil_spawn_subagents", map[string]any{
			"tasks": []map[string]any{{"label": "w", "prompt": "change it", "type": "writer"}},
		}),
	}
}

// A child that wrote and then failed left changed files behind. Both dispatch
// tools must still carry those paths to the parent's checkpoint; dropping them
// with the failure is a check that runs against nothing and passes.
func TestAFailedChildsWritesStillReachTheParent(t *testing.T) {
	for name, runner := range map[string]*partialRunner{
		"runner error":  {wrote: []string{"a.go", "b.go"}, err: errors.New("provider went away")},
		"empty summary": {wrote: []string{"a.go", "b.go"}},
	} {
		for tool, res := range dispatchBoth(t, runner) {
			if !res.IsError {
				t.Errorf("%s/%s: a wholly failed dispatch was not an error: %s", name, tool, res.Text)
			}
			if !slices.Equal(res.Wrote, []string{"a.go", "b.go"}) {
				t.Errorf("%s/%s: Wrote = %v; the failed child's changed files never reached the parent",
					name, tool, res.Wrote)
			}
			var payload struct {
				Results []struct {
					Status string   `json:"status"`
					Wrote  []string `json:"wrote"`
				} `json:"results"`
			}
			if err := json.Unmarshal([]byte(res.Text), &payload); err != nil || len(payload.Results) != 1 {
				t.Fatalf("%s/%s: unreadable payload %q: %v", name, tool, res.Text, err)
			}
			if got := payload.Results[0]; got.Status != "failed" || !slices.Equal(got.Wrote, []string{"a.go", "b.go"}) {
				t.Errorf("%s/%s: the model reads outcome %+v; want status failed naming a.go and b.go",
					name, tool, got)
			}
		}
	}
}

// A failed child that reported its own list as cut must leave the parent's
// list marked cut.
func TestAFailedChildsTruncationStillReachesTheParent(t *testing.T) {
	runner := &partialRunner{wrote: []string{"a.go"}, truncated: true, err: errors.New("step limit")}
	for tool, res := range dispatchBoth(t, runner) {
		if len(res.Degraded) == 0 {
			t.Errorf("%s: a failed child's truncated list was reported upward as complete", tool)
		}
	}
}

// A failed child that wrote nothing contributes nothing, and the failure is not
// mistaken for a partial success.
func TestAFailedChildThatWroteNothingReportsNoPaths(t *testing.T) {
	runner := &partialRunner{err: errors.New("could not start")}
	for tool, res := range dispatchBoth(t, runner) {
		if !res.IsError || len(res.Wrote) != 0 {
			t.Errorf("%s: IsError=%v Wrote=%v, want an error with no paths", tool, res.IsError, res.Wrote)
		}
	}
}

// A failed child still spent what it spent, and the dispatch reports it: a
// fan-out that counts only completed children under-reports exactly the turns
// an operator is investigating.
func TestAFailedChildsSpendIsReported(t *testing.T) {
	runner := &partialRunner{
		usage: SubAgentUsage{InputTokens: 1200, OutputTokens: 300},
		err:   errors.New("provider went away"),
	}
	for tool, res := range dispatchBoth(t, runner) {
		var payload struct {
			Usage *SubAgentUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
			t.Fatalf("%s: unreadable payload %q: %v", tool, res.Text, err)
		}
		if payload.Usage == nil || payload.Usage.InputTokens != 1200 || payload.Usage.OutputTokens != 300 {
			t.Errorf("%s: usage = %+v, want the failed child's 1200 in / 300 out", tool, payload.Usage)
		}
	}
}
