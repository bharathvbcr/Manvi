package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// scriptedRunner answers each child by its label, so one fan-out can mix
// children that succeed, fail after writing, fail clean and overflow the cap.
type scriptedRunner map[string]struct {
	out SubAgentResult
	err error
}

func (r scriptedRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	s := r[req.Label]
	return s.out, s.err
}

// A mixed fan-out under concurrency: the children run in parallel, and what
// reaches the parent must still be exactly the dispatch-order union of every
// path any child left behind — failed or not — de-duplicated, cut at the
// loop's cap, and saying so. Repeated so scheduling order gets a chance to
// matter; run it under -race for the data-race half of the claim.
func TestAMixedFanOutReportsEveryChildsWritesInDispatchOrder(t *testing.T) {
	flood := make([]string, 0, 300)
	for i := range 300 {
		flood = append(flood, fmt.Sprintf("flood/%03d.go", i))
	}
	ok := func(paths ...string) SubAgentResult {
		return SubAgentResult{Summary: "done", Wrote: paths}
	}
	runner := scriptedRunner{
		"c0": {out: ok("shared.go", "a0.go")},
		"c1": {out: SubAgentResult{Wrote: []string{"shared.go", "a1.go"}}, err: errors.New("provider went away")},
		"c2": {err: errors.New("could not start")},
		"c3": {out: SubAgentResult{Wrote: []string{"a3.go", " "}}}, // ran, wrote, no summary
		"c4": {out: ok("a4.go")},
		"c5": {out: SubAgentResult{Summary: "done", Wrote: flood, WroteTruncated: true}},
	}
	want := append([]string{"shared.go", "a0.go", "a1.go", "a3.go", "a4.go"}, flood...)[:tools.MaxTrackedWrites]

	var tasks []map[string]any
	for _, label := range []string{"c0", "c1", "c2", "c3", "c4", "c5"} {
		tasks = append(tasks, map[string]any{"label": label, "prompt": "work"})
	}
	for round := range 20 {
		f, _ := fixtureWithRoles(t, runner)
		res := f.call("devcouncil_spawn_subagents", map[string]any{"tasks": tasks})
		if res.IsError {
			t.Fatalf("round %d: a partly failed fan-out was reported as a total failure: %s", round, res.Text)
		}
		if !slices.Equal(res.Wrote, want) {
			t.Fatalf("round %d: Wrote has %d paths, first %q; want the dispatch-order union of %d",
				round, len(res.Wrote), res.Wrote[:min(6, len(res.Wrote))], len(want))
		}
		if len(res.Degraded) == 0 {
			t.Fatalf("round %d: the list was cut at %d and nothing says so", round, tools.MaxTrackedWrites)
		}
	}
}
