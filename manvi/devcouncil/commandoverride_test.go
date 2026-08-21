package devcouncil

import (
	"encoding/json"
	"testing"

	"manvi/flags"
	"manvi/policy"
	"manvi/ui"
)

// The tests in this file all guard one invariant, from different sides: the
// recovery a block hands an agent must be a recovery that works.
//
// It did not. A blocked shell command reported the command line in `target`, and
// the advice built from that decision named devcouncil_request_override with the
// command line under an argument called `path`. Following it produced
// `granted: true` for scope.unplanned — a rule the caller had not hit — appended
// the command text to the task's planned files for the rest of its life, and
// left the command blocked. Every step reported success and nothing changed.

// blockCommand runs an unlisted command and returns its refusal payload.
func blockCommand(t *testing.T, f *fixture, command string) map[string]any {
	t.Helper()
	res := f.call("devcouncil_exec_command", map[string]string{"command": command})
	if !res.IsError {
		t.Fatalf("exec_command(%q) was expected to be refused, got: %s", command, res.Text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("refusal payload is unparseable: %v (%q)", err, res.Text)
	}
	return payload
}

func plannedPaths(t *testing.T, f *fixture) []string {
	t.Helper()
	task := f.payload("devcouncil_get_task", map[string]any{"task_id": "TASK-001"})
	raw, _ := task["planned_files"].([]any)
	var out []string
	for _, entry := range raw {
		if m, ok := entry.(map[string]any); ok {
			if p, ok := m["path"].(string); ok {
				out = append(out, p)
			}
		}
	}
	return out
}

func checkout(t *testing.T, f *fixture) {
	t.Helper()
	if out := f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"}); out["acquired"] != true {
		t.Fatalf("checkout = %v", out)
	}
}

// TestCommandBlockNamesNoDeadEndRecovery is the assertion the defect failed. A
// named tool has to be one this agent can actually use.
func TestCommandBlockNamesNoDeadEndRecovery(t *testing.T) {
	f := newFixture(t)
	checkout(t, f)

	blocked := blockCommand(t, f, seedGrep)

	if blocked["rule"] != string(policy.RuleCommandNotAllowed) {
		t.Fatalf("rule = %v, want %v", blocked["rule"], policy.RuleCommandNotAllowed)
	}
	if blocked["subject"] != string(policy.SubjectCommand) {
		t.Errorf("subject = %v, want %q — target is a command line", blocked["subject"], policy.SubjectCommand)
	}
	if blocked["agent_grantable"] != false {
		t.Errorf("agent_grantable = %v; no agent-issued grant clears a command block by default",
			blocked["agent_grantable"])
	}
	if tool, named := blocked["suggested_tool"]; named {
		t.Errorf("named %v as the recovery for a block no agent-issued grant can clear", tool)
	}
	if _, named := blocked["suggested_arguments"]; named {
		t.Error("supplied override arguments for a block the agent cannot override")
	}
	// The refusal has to leave the agent somewhere to go, and the place it goes
	// is the tool surface that was never gated in the first place.
	alts, _ := blocked["ungated_alternatives"].([]any)
	if len(alts) == 0 {
		t.Error("no ungated alternative was offered for a blocked command")
	}
	for _, alt := range alts {
		name, _ := alt.(string)
		if !f.pipe.Has(name) {
			t.Errorf("offered %q as an alternative, but no such tool is registered", name)
		}
	}
}

// TestFollowingCommandRecoveryDoesNotForgeAGrantForAnotherRule pins the worst of
// the observed behaviour: a grant reported for a rule nobody hit.
func TestFollowingCommandRecoveryDoesNotForgeAGrantForAnotherRule(t *testing.T) {
	f := newFixture(t)
	checkout(t, f)

	blocked := blockCommand(t, f, seedGrep)
	before := plannedPaths(t, f)

	// Ask for the override the old advice would have produced, in the old shape:
	// the command line in `path`. This is what a replayed transcript still sends.
	out := f.payload("devcouncil_request_override", map[string]string{
		"path":   blocked["target"].(string),
		"rule":   blocked["rule"].(string),
		"reason": "need to search the tree to locate the defect",
	})

	if out["granted"] == true {
		t.Errorf("granted a command override the ledger does not permit an agent to issue: %v", out)
	}
	if rule, ok := out["rule"].(string); ok && rule != string(policy.RuleCommandNotAllowed) {
		t.Errorf("grant decision was about %q; the caller was blocked by %q",
			rule, policy.RuleCommandNotAllowed)
	}
	if out["scope_persisted"] != nil || out["scope_appended"] != nil {
		t.Errorf("a command override touched the task's file scope: %v", out)
	}
	if after := plannedPaths(t, f); len(after) != len(before) {
		t.Errorf("planned files went from %v to %v; a command line is not a planned file", before, after)
	}
	if out["suggested_action"] == nil {
		t.Error("a refusal must say which authority could clear it")
	}
}

// TestAgentCommandGrantFlagIsLive covers the other half: the operator flag that
// permits these grants was unreachable, so turning it on changed nothing.
func TestAgentCommandGrantFlagIsLive(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureStrict,
		flags.GrantsAgentCommands: "true",
	})
	checkout(t, f)

	blocked := blockCommand(t, f, seedGrep)
	if blocked["agent_grantable"] != true {
		t.Fatalf("agent_grantable = %v with %s=true", blocked["agent_grantable"], flags.GrantsAgentCommands)
	}
	tool, _ := blocked["suggested_tool"].(string)
	if tool != "devcouncil_request_override" {
		t.Fatalf("suggested_tool = %q, want the override tool once the operator has enabled it", tool)
	}
	args, _ := blocked["suggested_arguments"].(map[string]any)
	if _, wrong := args["path"]; wrong {
		t.Errorf("override arguments named `path` for a command block: %v", args)
	}
	if args["command"] != blocked["target"] {
		t.Errorf("override arguments carry command %v, block target was %v", args["command"], blocked["target"])
	}

	// Follow the advice exactly as given.
	followed := map[string]any{"reason": "need to search the tree to locate the defect"}
	for k, v := range args {
		followed[k] = v
	}
	out := f.payload(tool, followed)
	if out["granted"] != true {
		t.Fatalf("override refused with the flag on: %v", out)
	}
	if out["rule"] != string(policy.RuleCommandNotAllowed) {
		t.Errorf("granted rule = %v, want %v", out["rule"], policy.RuleCommandNotAllowed)
	}
	if out["scope_persisted"] != nil {
		t.Errorf("a command grant wrote into file scope: %v", out)
	}

	// And the advice has to have accomplished something. A command that runs and
	// fails is a different outcome from one the gate refused, so the assertion is
	// on the policy decision, not on the exit code.
	res := f.call("devcouncil_exec_command", map[string]string{"command": seedGrep})
	if denial, blocked := policyDenial(res.Text); blocked {
		t.Fatalf("command still refused by %s after the grant it was told to ask for: %s",
			denial, res.Text)
	}
	if res.IsError {
		t.Fatalf("granted command did not run cleanly: %s", res.Text)
	}
}

// seedGrep is unlisted by every allowlist, and succeeds against the file the
// fixture commits, so a refusal and a non-zero exit cannot be confused.
const seedGrep = "grep -c seed seed.txt"

// policyDenial reports whether a tool result is a gate refusal, and which rule
// refused it. An exec failure is not a refusal: the command ran.
func policyDenial(text string) (string, bool) {
	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		return "", false
	}
	if payload["action"] != string(policy.Deny) {
		return "", false
	}
	rule, _ := payload["rule"].(string)
	return rule, true
}

// TestCommandWithoutLeaseSendsTheAgentToCheckout guards the command-gate twin of
// task.absent, which used to be routed to a tool that first demands the very
// lease the rule reports missing.
func TestCommandWithoutLeaseSendsTheAgentToCheckout(t *testing.T) {
	f := newFixture(t)

	blocked := blockCommand(t, f, seedGrep)
	if blocked["rule"] != string(policy.RuleCommandNoLease) {
		t.Fatalf("rule = %v, want %v", blocked["rule"], policy.RuleCommandNoLease)
	}
	if blocked["suggested_tool"] != "devcouncil_checkout_task" {
		t.Errorf("suggested_tool = %v, want devcouncil_checkout_task", blocked["suggested_tool"])
	}
	if blocked["subject"] != string(policy.SubjectCommand) {
		t.Errorf("subject = %v, want command", blocked["subject"])
	}
}

// TestUnknownRuleNameIsRefusedRatherThanInterpreted closes the way back in: an
// unclassified rule name would fall to the file gate, whose soft rules are the
// agent-grantable ones.
func TestUnknownRuleNameIsRefusedRatherThanInterpreted(t *testing.T) {
	f := newFixture(t)
	checkout(t, f)

	before := plannedPaths(t, f)
	res := f.call("devcouncil_request_override", map[string]string{
		"path":   "grep -rn package src",
		"rule":   "command.not_allowd", // one letter short
		"reason": "typo in the rule name",
	})
	if !res.IsError {
		t.Fatalf("a misspelled rule was accepted: %s", res.Text)
	}
	if after := plannedPaths(t, f); len(after) != len(before) {
		t.Errorf("a refused override still widened scope: %v -> %v", before, after)
	}
}

// TestEverySuggestedOverrideIsActionable is the general form, over every soft
// block the tool surface can produce. If the payload names the override tool,
// the agent must be permitted to use it.
func TestEverySuggestedOverrideIsActionable(t *testing.T) {
	for _, allowCommands := range []string{"false", "true"} {
		t.Run("allow_commands="+allowCommands, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{
				flags.HarnessPosture:      flags.PostureStrict,
				flags.GrantsAgentCommands: allowCommands,
			})
			checkout(t, f)

			payloads := []map[string]any{
				blockCommand(t, f, seedGrep),
				f.payload("devcouncil_policy_check_write", map[string]string{"path": "other/deep/x.go"}),
				f.payload("devcouncil_policy_check_write", map[string]string{"path": "package.json"}),
			}
			for _, p := range payloads {
				if p["allowed"] == true {
					continue
				}
				tool, named := p["suggested_tool"].(string)
				if !named || tool != "devcouncil_request_override" {
					continue
				}
				if p["agent_grantable"] != true {
					t.Errorf("rule %v names the override tool but agent_grantable=%v",
						p["rule"], p["agent_grantable"])
				}
				args, _ := p["suggested_arguments"].(map[string]any)
				subject, _ := p["subject"].(string)
				switch subject {
				case string(policy.SubjectCommand):
					if _, ok := args["command"]; !ok {
						t.Errorf("rule %v has a command subject but its arguments are %v", p["rule"], args)
					}
				case string(policy.SubjectPath):
					if _, ok := args["path"]; !ok {
						t.Errorf("rule %v has a path subject but its arguments are %v", p["rule"], args)
					}
				default:
					t.Errorf("rule %v reported no subject", p["rule"])
				}
			}
		})
	}
}

// TestTheOperatorIsAskedAboutTheRightKindOfThing guards the human end of the
// same invariant. A blocked shell command was escalated with the command line in
// a field the card renders as "path", so the question an operator answered named
// a different act than the one being performed.
func TestTheOperatorIsAskedAboutTheRightKindOfThing(t *testing.T) {
	approver := &recordingApprover{decision: ui.Decision{Allow: false, Reason: "declined"}}
	f := newFixture(t).withApprover(approver)
	checkout(t, f)

	f.call("devcouncil_exec_command", map[string]string{"command": seedGrep})
	f.call("devcouncil_write_file", map[string]string{"path": "other/deep/x.go", "content": "x"})

	if len(approver.asked) < 2 {
		t.Fatalf("expected both a command and a write to be escalated, got %d", len(approver.asked))
	}
	for _, req := range approver.asked {
		want := string(policy.SubjectOf(policy.RuleID(req.Rule)))
		if req.Subject != want {
			t.Errorf("rule %q escalated with subject %q, want %q", req.Rule, req.Subject, want)
		}
		if req.SubjectLabel() == "path" && policy.IsCommandRule(policy.RuleID(req.Rule)) {
			t.Errorf("a command block was put to a human under the label %q: %q",
				req.SubjectLabel(), req.Path)
		}
	}
}
