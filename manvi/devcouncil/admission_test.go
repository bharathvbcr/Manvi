package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

var errCapRefused = errors.New("budget exhausted")

// capBudget answers Headroom from a script, one value per call; a call past
// the end repeats the last.
type capBudget struct {
	mu       sync.Mutex
	headroom []int64
	calls    int
}

func (b *capBudget) Headroom(context.Context) (int64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.calls
	b.calls++
	return b.headroom[min(n, len(b.headroom)-1)], nil
}

func (b *capBudget) Exhausted(err error) bool   { return errors.Is(err, errCapRefused) }
func (b *capBudget) Render(amount int64) string { return fmt.Sprintf("$%d", amount) }

// cappedRunner is a runner whose children draw on a cap: each reserves 30 for
// its first request. The child labelled "first" writes a file and is then
// refused by the cap mid-turn; every other child succeeds.
type cappedRunner struct {
	budget *capBudget
	ran    atomic.Int32
}

func (r *cappedRunner) AdmitSubAgents(reqs []SubAgentRequest) (agents.Budget, []int64, error) {
	reserves := make([]int64, len(reqs))
	for i := range reserves {
		reserves[i] = 30
	}
	return r.budget, reserves, nil
}

func (r *cappedRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	r.ran.Add(1)
	if req.Label == "first" {
		return SubAgentResult{
			Wrote: []string{"half.go"},
			Usage: SubAgentUsage{Model: "anthropic/m", InputTokens: 10, OutputTokens: 2},
		}, fmt.Errorf("step 2: %w", errCapRefused)
	}
	return SubAgentResult{Summary: "done", Usage: SubAgentUsage{Model: "anthropic/m", InputTokens: 10}}, nil
}

var dispatchTools = []string{"invoke_subagent", "spawn_subagents"}

// dispatchThree sends one three-child fan-out through the named dispatch tool.
func dispatchThree(t *testing.T, tool string, runner SubAgentRunner) tools.Result {
	t.Helper()
	f, _ := fixtureWithRoles(t, runner, agents.Definition{Name: "writer", Role: "Writer", EnableWriteTools: true})
	var invoke, spawn []map[string]any
	for _, label := range []string{"first", "second", "third"} {
		invoke = append(invoke, map[string]any{"type_name": "writer", "role": label, "prompt": "change it"})
		spawn = append(spawn, map[string]any{"label": label, "prompt": "change it", "type": "writer"})
	}
	if tool == "invoke_subagent" {
		return f.call("devcouncil_invoke_subagent", map[string]any{"subagents": invoke})
	}
	return f.call("devcouncil_spawn_subagents", map[string]any{"tasks": spawn})
}

// TestBothDispatchersRefuseAFanOutWhoseChildrenCannotFitAlone. Three children at
// 30 against a headroom of 20: none can ever fit, so none may start, and the
// refusal carries the numbers rather than a bare "budget". (A headroom of 60
// no longer refuses: the pool narrows the fan-out to two at a time.)
func TestBothDispatchersRefuseAFanOutWhoseChildrenCannotFitAlone(t *testing.T) {
	for _, tool := range dispatchTools {
		runner := &cappedRunner{budget: &capBudget{headroom: []int64{20}}}
		res := dispatchThree(t, tool, runner)
		if !res.IsError {
			t.Errorf("%s: a fan-out the cap cannot cover was dispatched: %s", tool, res.Text)
		}
		if runner.ran.Load() != 0 {
			t.Errorf("%s: %d children ran; a refused fan-out starts none", tool, runner.ran.Load())
		}
		for _, want := range []string{"first", "$30", "$20", "3 metered children", "$90"} {
			if !strings.Contains(res.Text, want) {
				t.Errorf("%s: refusal %q does not say %q", tool, res.Text, want)
			}
		}
	}
}

// TestBudgetExhaustedMidFanOutReportsTheRestAsNotStarted.
//
// The up-front check passes: three children at 30 fit in 100. Then the cap
// runs out mid fan-out — the first child starts, writes half.go and has its
// next request refused, and by the time the second is to be admitted the
// ledger shows 10 left. The other two must not be started, and what the
// dispatching model reads for them is "not started (budget)" — never a failure
// carrying writes, because they made none. half.go, which the started child
// really did write, still reaches the parent's checkpoint.
//
// The order is fixed by the headroom script, not by the scheduler: call 0 is
// the up-front check, call 1 admits "first", call 2 admits "second" and sees
// 10. (The pool's own sibling-refused path is pinned in package agents, where
// the test can hold an admission until a child's lease has come back.)
func TestBudgetExhaustedMidFanOutReportsTheRestAsNotStarted(t *testing.T) {
	for _, tool := range dispatchTools {
		runner := &cappedRunner{budget: &capBudget{headroom: []int64{100, 100, 10}}}
		res := dispatchThree(t, tool, runner)

		if runner.ran.Load() != 1 {
			t.Fatalf("%s: %d children ran; nothing may start after the cap refused one", tool, runner.ran.Load())
		}
		if !res.IsError {
			t.Errorf("%s: nothing succeeded, yet the dispatch was not an error: %s", tool, res.Text)
		}
		if !slices.Equal(res.Wrote, []string{"half.go"}) {
			t.Errorf("%s: Wrote = %v; the started child's write must still reach the parent", tool, res.Wrote)
		}
		var payload struct {
			Report  agents.Report `json:"report"`
			Clean   bool          `json:"clean"`
			Results []struct {
				Label  string   `json:"label"`
				Status string   `json:"status"`
				Reason string   `json:"reason"`
				Error  string   `json:"error"`
				Wrote  []string `json:"wrote"`
				Usage  *SubAgentUsage
			} `json:"results"`
		}
		if err := json.Unmarshal([]byte(res.Text), &payload); err != nil || len(payload.Results) != 3 {
			t.Fatalf("%s: unreadable payload %q: %v", tool, res.Text, err)
		}
		if payload.Report.Failed != 1 || payload.Report.NotStarted != 2 || payload.Clean {
			t.Errorf("%s: report = %+v clean=%v; want 1 failed, 2 not started, not clean", tool, payload.Report, payload.Clean)
		}
		if got := payload.Results[0]; got.Status != "failed" || !slices.Equal(got.Wrote, []string{"half.go"}) {
			t.Errorf("%s: first = %+v; want failed, naming half.go", tool, got)
		}
		for _, got := range payload.Results[1:] {
			if got.Status != "not_started" || got.Reason != agents.NotStartedBudget ||
				!strings.HasPrefix(got.Error, "not started (budget)") {
				t.Errorf("%s: %s = %+v; want not_started for budget", tool, got.Label, got)
			}
			if len(got.Wrote) != 0 || got.Usage != nil {
				t.Errorf("%s: %s never started, yet reports writes %v or spend %+v", tool, got.Label, got.Wrote, got.Usage)
			}
		}
	}
}

// TestARunnerWithoutACapDispatchesAsBefore. A runner that does not implement
// SubAgentAdmitter has no cap to admit against, and its fan-out must neither be
// narrowed nor refused for want of one.
func TestARunnerWithoutACapDispatchesAsBefore(t *testing.T) {
	runner := scriptedRunner{
		"first":  {out: SubAgentResult{Summary: "done"}},
		"second": {out: SubAgentResult{Summary: "done"}},
		"third":  {out: SubAgentResult{Summary: "done"}},
	}
	for _, tool := range dispatchTools {
		res := dispatchThree(t, tool, runner)
		if res.IsError || !strings.Contains(res.Text, `"clean":true`) {
			t.Errorf("%s: an uncapped fan-out = %s", tool, res.Text)
		}
	}
}
