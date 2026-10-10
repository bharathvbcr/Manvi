package devcouncil

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

func TestEscalationBudgetIsCeilHalfAndSingleFlight(t *testing.T) {
	for n, want := range map[int]int{0: 0, 1: 1, 2: 1, 3: 2, 8: 4, 9: 5, -3: 0} {
		if got := NewEscalationBudget(n).Cap(); got != want {
			t.Errorf("NewEscalationBudget(%d).Cap() = %d, want %d", n, got, want)
		}
	}
	var nilBudget *EscalationBudget
	if nilBudget.Take() || nilBudget.Cap() != 0 {
		t.Error("a nil budget allowed an escalation")
	}
	b := NewEscalationBudget(64)
	var taken atomic.Int32
	var wg sync.WaitGroup
	for range 256 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.Take() {
				taken.Add(1)
			}
		}()
	}
	wg.Wait()
	if taken.Load() != 32 {
		t.Fatalf("%d escalations taken from a cap of 32", taken.Load())
	}
}

func TestEscalationErrorsAreTyped(t *testing.T) {
	cause := errors.New("529")
	var err error = &EscalationError{Child: "c", From: agents.TierLite, To: agents.TierHeavy, Evidence: "e", Err: cause}
	if !errors.Is(err, cause) {
		t.Error("EscalationError hides its cause")
	}
	ex := &ExhaustedError{Child: "c", From: agents.TierLite, To: agents.TierHeavy, Evidence: "e"}
	if !strings.Contains(ex.Error(), "lite") || !strings.Contains(ex.Error(), "heavy") {
		t.Errorf("ExhaustedError does not name its tiers: %s", ex)
	}
}

// escalatingRunner records the budget each child was handed and reports one
// escalation from the child labelled "second".
type escalatingRunner struct {
	mu      sync.Mutex
	budgets []*EscalationBudget
}

func (r *escalatingRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	r.mu.Lock()
	r.budgets = append(r.budgets, req.Escalation)
	r.mu.Unlock()
	out := SubAgentResult{Summary: "done"}
	if req.Label == "second" {
		out.Escalations = []Escalation{{
			Child: "second", From: agents.TierLite, To: agents.TierHeavy,
			Evidence: "an empty summary after 3 step(s)", ColdCache: true,
		}}
	}
	return out, nil
}

// Every child of one dispatch draws on the same cap, sized ceil(N/2), through
// both dispatch tools; and the report names each escalation.
func TestBothDispatchersShareOneEscalationCapAndReportEscalations(t *testing.T) {
	for _, tool := range dispatchTools {
		runner := &escalatingRunner{}
		res := dispatchThree(t, tool, runner)
		if res.IsError {
			t.Fatalf("%s: %s", tool, res.Text)
		}
		if len(runner.budgets) != 3 {
			t.Fatalf("%s: %d children ran", tool, len(runner.budgets))
		}
		for i, b := range runner.budgets {
			if b == nil || b != runner.budgets[0] {
				t.Fatalf("%s: child %d was handed a different escalation budget", tool, i)
			}
		}
		if got := runner.budgets[0].Cap(); got != 2 {
			t.Errorf("%s: cap %d for three children, want 2", tool, got)
		}
		for _, want := range []string{`"escalations"`, `"child":"second"`, `"from":"lite"`, `"to":"heavy"`,
			"an empty summary after 3 step(s)", `"escalation_cap":2`} {
			if !strings.Contains(res.Text, want) {
				t.Errorf("%s: the report does not say %s:\n%s", tool, want, res.Text)
			}
		}
	}
}

// captureRunner records every request it is handed.
type captureRunner struct {
	mu   sync.Mutex
	reqs []SubAgentRequest
}

func (r *captureRunner) RunSubAgent(_ context.Context, req SubAgentRequest) (SubAgentResult, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	return SubAgentResult{Summary: "done"}, nil
}

// One role owner for both dispatchers and the tier rule over it: a type no one
// registered stays non-writing and capped at lite whichever tool names it, and
// a per-call model that is a raw id is refused, even on a role that is not
// registered (the fail-open branch once appended a raw override to placements).
func TestAnUnknownRoleIsReadOnlyAtLiteOnBothDispatchersAndNeverTakesARawModel(t *testing.T) {
	for _, tool := range dispatchTools {
		runner := &captureRunner{}
		f, _ := fixtureWithRoles(t, runner)
		var out string
		if tool == "invoke_subagent" {
			r := f.call("devcouncil_invoke_subagent", map[string]any{"subagents": []map[string]any{
				{"type_name": "no-such-role", "role": "x", "prompt": "p"}}})
			out = r.Text
		} else {
			r := f.call("devcouncil_spawn_subagents", map[string]any{"tasks": []map[string]any{
				{"label": "x", "prompt": "p", "type": "no-such-role"}}})
			out = r.Text
		}
		if len(runner.reqs) != 1 {
			t.Fatalf("%s: %d children ran: %s", tool, len(runner.reqs), out)
		}
		req := runner.reqs[0]
		if !req.ReadOnly || req.Tier != agents.TierLite || req.ModelSpec != "" {
			t.Errorf("%s: unknown role child = readOnly %v tier %q model %q, want read-only, lite, no model",
				tool, req.ReadOnly, req.Tier, req.ModelSpec)
		}
		if !strings.Contains(out, "no-such-role") {
			t.Errorf("%s: the unknown role is not reported: %s", tool, out)
		}
	}

	runner := &captureRunner{}
	f, _ := fixtureWithRoles(t, runner)
	res := f.call("devcouncil_invoke_subagent", map[string]any{"subagents": []map[string]any{
		{"type_name": "no-such-role", "role": "x", "prompt": "p", "model": "claude-fable-5-1"}}})
	if !res.IsError || len(runner.reqs) != 0 {
		t.Fatalf("a raw model id on an unknown role was dispatched: %v %s", len(runner.reqs), res.Text)
	}
}
