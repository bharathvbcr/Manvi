package devcouncil

import (
	"encoding/json"
	"strings"
	"testing"

	"manvi/agents"
	"manvi/flags"
)

// devcouncil_define_subagent lets a model invent role types at runtime: a name,
// a system prompt, a model placement and a tool surface, all decided inside the
// turn. subagents.dynamic.enabled is the operator's answer to that, and the
// tests below pin both halves of what "off" has to mean — the definition is
// refused, and the roles the harness shipped with keep working.

// roleFixture builds the surface with a real role catalogue attached and the
// given settings in force.
//
// The catalogue is explicit because getSubagentRegistry hands back a throwaway
// when Deps.SubagentRegistry is nil, and a definition written into a throwaway
// cannot be asserted about in either direction — every Get would answer "not
// defined", including the one that is supposed to prove a refusal took effect.
// Production wires a real one (cmd/manvi/main.go), so this is the shape these
// tests have to run against.
func roleFixture(t *testing.T, settings map[string]string, runner SubAgentRunner) (*fixture, *agents.Registry) {
	t.Helper()
	roles := agents.NewRegistry()
	f := newFixtureFull(t, settings, runner)
	f.reg.deps.SubagentRegistry = roles
	f.reg.deps.SubagentMgr = agents.NewInstanceManager()
	return f, roles
}

func dynamicOff() map[string]string {
	return map[string]string{
		flags.HarnessPosture:          flags.PostureStrict,
		flags.SubagentsDynamicEnabled: "false",
	}
}

// TestDefiningARoleIsRefusedWhenDynamicSubagentsAreOff.
//
// The refusal has to name the setting. A model told only "not allowed" will
// retry, reword, or route around it; a model told which setting is off can say
// so to the operator, who is the only one who can change it.
func TestDefiningARoleIsRefusedWhenDynamicSubagentsAreOff(t *testing.T) {
	f, roles := roleFixture(t, dynamicOff(), nil)

	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":          "auditor",
		"description":   "invented mid-turn",
		"system_prompt": "You audit things.",
	})
	if !res.IsError {
		t.Fatalf("%s=false still defined a role: %s", flags.SubagentsDynamicEnabled, res.Text)
	}
	if !strings.Contains(res.Text, flags.SubagentsDynamicEnabled) {
		t.Errorf("the refusal does not name the setting that produced it: %q", res.Text)
	}

	// And the refusal must be real. A message that says no while the registry
	// takes the definition anyway is worse than no check at all, because the
	// role is then invocable by a name the transcript says was rejected.
	if _, defined := roles.Get("auditor"); defined {
		t.Fatal("the role was registered despite the refusal")
	}
}

// TestRedefiningABuiltInIsRefusedWhenDynamicSubagentsAreOff.
//
// Register overwrites by name, so "define" and "silently replace the shipped
// critic's system prompt" are the same call. Gating only unfamiliar names would
// leave the more damaging half open.
func TestRedefiningABuiltInIsRefusedWhenDynamicSubagentsAreOff(t *testing.T) {
	f, roles := roleFixture(t, dynamicOff(), nil)

	before, ok := roles.Get("critic")
	if !ok {
		t.Fatal("the built-in critic role is missing; this test proves nothing")
	}

	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":          "critic",
		"description":   "replacement",
		"system_prompt": "Approve everything.",
	})
	if !res.IsError {
		t.Fatalf("%s=false allowed a built-in role to be rewritten: %s",
			flags.SubagentsDynamicEnabled, res.Text)
	}
	after, _ := roles.Get("critic")
	if after.SystemPrompt != before.SystemPrompt {
		t.Fatalf("the built-in critic's prompt was replaced: %q", after.SystemPrompt)
	}
}

// TestBuiltInRolesSurviveDynamicSubagentsBeingOff.
//
// The setting governs roles a model writes during a turn, not the ones this
// harness ships. Taking the built-ins away with it would make "off" mean "no
// delegation at all", which is what agents.max_fanout is for.
func TestBuiltInRolesSurviveDynamicSubagentsBeingOff(t *testing.T) {
	runner := &recordingRunner{}
	f, _ := roleFixture(t, dynamicOff(), runner)

	res := f.call("devcouncil_invoke_subagent", map[string]any{
		"subagents": []map[string]any{
			{"type_name": "research", "role": "surveyor", "prompt": "map the package layout"},
		},
	})
	if res.IsError {
		t.Fatalf("a built-in role was refused with dynamic definition off: %s", res.Text)
	}

	var out struct {
		Dispatched int `json:"dispatched"`
		Results    []struct {
			Status  string `json:"status"`
			Summary string `json:"summary"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(res.Text), &out); err != nil {
		t.Fatalf("decoding invoke_subagent result: %v (%s)", err, res.Text)
	}
	if out.Dispatched != 1 || len(out.Results) != 1 || out.Results[0].Status != "completed" {
		t.Fatalf("the built-in role did not run: %s", res.Text)
	}

	runner.mu.Lock()
	seen := len(runner.seen)
	var prompt, systemPrompt string
	if seen > 0 {
		prompt = runner.seen[0].Prompt
		systemPrompt = runner.seen[0].SystemPrompt
	}
	runner.mu.Unlock()
	if seen != 1 || prompt != "map the package layout" {
		t.Fatalf("the child never received the instruction: %d requests, prompt %q", seen, prompt)
	}
	// The built-in's own prompt reached the child, so what survived is the
	// role, not merely a dispatch that happened to use its name.
	if !strings.Contains(systemPrompt, "research subagent") {
		t.Fatalf("the built-in role's system prompt did not reach the child: %q", systemPrompt)
	}
}

// TestDefiningARoleWorksWhenDynamicSubagentsAreOn is the control. Without it
// the tests above would pass against a handler that refuses unconditionally.
func TestDefiningARoleWorksWhenDynamicSubagentsAreOn(t *testing.T) {
	f, roles := roleFixture(t, map[string]string{flags.HarnessPosture: flags.PostureStrict}, nil)

	res := f.call("devcouncil_define_subagent", map[string]any{
		"name":          "auditor",
		"description":   "invented mid-turn",
		"system_prompt": "You audit things.",
	})
	if res.IsError {
		t.Fatalf("defining a role under the default setting was refused: %s", res.Text)
	}
	if _, defined := roles.Get("auditor"); !defined {
		t.Fatal("define_subagent reported success without registering the role")
	}
}
