package devcouncil

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// D1. A per-call model is the dispatching model choosing where a child runs.
// It used to be passed through as any placement at all, so a research child —
// a read-only survey role — could be put on the most expensive model in the
// catalogue by naming it.
func TestInvokeSubagentRefusesARawModelIDOnResearch(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "research", "role": "surveyor", "prompt": "survey it", "model": "claude-fable-5-1"},
		},
	})
	if !res.IsError {
		t.Fatalf("a research child was placed on a raw model id: %s", res.Text)
	}
	for _, want := range []string{"claude-fable-5-1", "basic, lite"} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the refusal does not mention %q (the allowed tiers): %s", want, res.Text)
		}
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.seen) != 0 {
		t.Fatalf("the runner was asked for %d children after a refusal", len(runner.seen))
	}
}

// A tier above the role's ceiling is refused the same way: research may ask
// for basic or lite, never for the advisor.
func TestInvokeSubagentRefusesATierAboveTheRolesCeiling(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "builder", "role": "b", "prompt": "build it"},
			{"type_name": "research", "role": "r", "prompt": "survey it", "model": "advisor"},
		},
	})
	if !res.IsError || !strings.Contains(res.Text, "above") {
		t.Fatalf("a tier above research's ceiling was accepted: %s", res.Text)
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	// Refused whole: the builder beside it must not have run either.
	if len(runner.seen) != 0 {
		t.Fatalf("%d children ran from a dispatch that was refused", len(runner.seen))
	}
}

// D1 on the other path. A role a model defines decides every later dispatch
// of it, so its placement is held to the same rule.
func TestDefineSubagentRefusesARawModelID(t *testing.T) {
	f, roles := fixtureWithRoles(t, &recordingRunner{})

	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":               "pricey_reader",
		"description":        "reads things",
		"system_prompt":      "read",
		"model":              "claude-fable-5-1",
		"enable_write_tools": false,
	})
	if !res.IsError {
		t.Fatalf("a model-authored role naming a raw model id was registered: %s", res.Text)
	}
	if !strings.Contains(res.Text, "not a tier") || !strings.Contains(res.Text, "basic, lite") {
		t.Errorf("the refusal does not say a tier is required and which: %s", res.Text)
	}
	if _, ok := roles.Get("pricey_reader"); ok {
		t.Fatal("the refused role was registered anyway")
	}
}

// The ceiling is earned by the role's permissions, not declared: a read-only
// role may not claim heavy, and a writing one may not claim the advisor.
func TestDefineSubagentRefusesATierAboveItsCeiling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes bool
		key    string
		tier   string
	}{
		{name: "reader_claims_heavy", writes: false, key: "tier", tier: "heavy"},
		{name: "writer_claims_advisor", writes: true, key: "tier", tier: "advisor"},
		{name: "writer_claims_main_via_model", writes: true, key: "model", tier: "main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, roles := fixtureWithRoles(t, &recordingRunner{})
			res := f.call("devcouncil_define_subagent", map[string]any{
				"name":               tc.name,
				"description":        "d",
				"system_prompt":      "p",
				"enable_write_tools": tc.writes,
				tc.key:               tc.tier,
			})
			if !res.IsError || !strings.Contains(res.Text, "above") {
				t.Fatalf("%s=%q on a role with writes=%v was accepted: %s", tc.key, tc.tier, tc.writes, res.Text)
			}
			if _, ok := roles.Get(tc.name); ok {
				t.Fatal("the refused role was registered anyway")
			}
		})
	}
}

// A tier within the ceiling is accepted and stored as the role's tier, with no
// model placement of its own.
func TestDefineSubagentStoresATierWithinItsCeiling(t *testing.T) {
	f, roles := fixtureWithRoles(t, &recordingRunner{})
	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":               "cheap_builder",
		"description":        "d",
		"system_prompt":      "p",
		"enable_write_tools": true,
		"model":              "lite",
	})
	if res.IsError {
		t.Fatalf("define_subagent: %s", res.Text)
	}
	def, ok := roles.Get("cheap_builder")
	if !ok {
		t.Fatal("the role was not registered")
	}
	if def.Tier != agents.TierLite || !agents.ParsePlacement(def.Model, nil).Inherits() {
		t.Fatalf("stored tier=%q model=%q, want tier lite and no model placement", def.Tier, def.Model)
	}
}

// The router reaches the dispatch: in one fan-out a builder is heavy and
// read-only children are basic once the fan-out is wide, lite while it is not.
func TestDispatchRoutesEachChildToItsTier(t *testing.T) {
	children := func(research int) []map[string]any {
		subs := []map[string]any{{"type_name": "builder", "role": "builder", "prompt": "build"}}
		for i := 0; i < research; i++ {
			subs = append(subs, map[string]any{"type_name": "research", "role": "research", "prompt": "survey"})
		}
		return subs
	}
	for _, tc := range []struct {
		research int
		want     agents.Tier
	}{
		{research: 7, want: agents.TierBasic},
		{research: agents.BasicFanoutWidth - 2, want: agents.TierLite},
	} {
		runner := &recordingRunner{}
		f, _ := fixtureWithRoles(t, runner)
		if res := f.call("devcouncil_invoke_subagent", map[string]any{"subagents": children(tc.research)}); res.IsError {
			t.Fatalf("invoke_subagent: %s", res.Text)
		}
		runner.mu.Lock()
		for _, req := range runner.seen {
			want := tc.want
			if req.Label == "builder" {
				want = agents.TierHeavy
			}
			if req.Tier != want {
				t.Errorf("width %d: %s child routed to %q, want %q", tc.research+1, req.Label, req.Tier, want)
			}
		}
		runner.mu.Unlock()
	}

	// The other dispatcher routes the same way, including a task with no role.
	runner := &recordingRunner{}
	f, _ := fixtureWithRoles(t, runner)
	if res := f.call("devcouncil_spawn_subagents", map[string]any{
		"tasks": []map[string]any{
			{"label": "writer", "prompt": "do it"},
			{"label": "reader", "prompt": "look", "read_only": true},
			{"label": "planner", "prompt": "plan", "type": "planner"},
		},
	}); res.IsError {
		t.Fatalf("spawn_subagents: %s", res.Text)
	}
	want := map[string]agents.Tier{"writer": agents.TierHeavy, "reader": agents.TierLite, "planner": agents.TierLite}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, req := range runner.seen {
		if req.Tier != want[req.Label] {
			t.Errorf("spawn: %s routed to %q, want %q", req.Label, req.Tier, want[req.Label])
		}
	}
}
