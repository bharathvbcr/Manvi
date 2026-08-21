package devcouncil

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"manvi/flags"
	"manvi/policy"
)

// FuzzOverrideSubject drives the router that decides which gate an override
// request is about. Its contract is narrow and total: the subject follows the
// rule whenever the rule is one the engine declares, and the target is always
// something the caller actually sent — never fabricated, never the other field
// when the rule was explicit about which one it wanted.
func FuzzOverrideSubject(f *testing.F) {
	for _, seed := range [][3]string{
		{"command.not_allowed", "", "grep -rn x ."},
		{"command.not_allowed", "grep -rn x .", ""},
		{"scope.unplanned", "src/a.go", ""},
		{"scope.unplanned", "", "src/a.go"},
		{"", "", "ls"},
		{"", "src/a.go", ""},
		{"", "", ""},
		{"  command.no_lease  ", "  ", "  git log  "},
	} {
		f.Add(seed[0], seed[1], seed[2])
	}

	f.Fuzz(func(t *testing.T, rule, path, command string) {
		subject, target := overrideSubject(rule, command, path)

		if subject != policy.SubjectPath && subject != policy.SubjectCommand {
			t.Fatalf("overrideSubject(%q,%q,%q) invented subject %q", rule, command, path, subject)
		}

		// The target is never fabricated.
		if target != "" && target != strings.TrimSpace(path) && target != strings.TrimSpace(command) {
			t.Fatalf("overrideSubject(%q,%q,%q) target %q came from neither argument",
				rule, command, path, target)
		}

		// A declared rule decides the subject outright.
		id := policy.RuleID(strings.TrimSpace(rule))
		if policy.RuleKnown(id) {
			want := policy.SubjectOf(id)
			if subject != want {
				t.Fatalf("rule %q is classified %q but router chose %q", id, want, subject)
			}
		}

		// Deterministic: the router is consulted more than once per request.
		if s2, t2 := overrideSubject(rule, command, path); s2 != subject || t2 != target {
			t.Fatalf("overrideSubject is not deterministic: (%q,%q) then (%q,%q)", subject, target, s2, t2)
		}
	})
}

// TestAGrantThatReportsSuccessChangesTheOutcome is the end-to-end invariant the
// defect violated in the loudest possible way: the agent was told "granted" and
// the subject it named was refused again on the very next call.
//
// It is asserted over every soft block the tool surface can produce, under both
// settings of the operator flag, because the failure was not in one branch — it
// was in advice built without knowing which gate it was talking about.
func TestAGrantThatReportsSuccessChangesTheOutcome(t *testing.T) {
	subjects := []struct {
		name    string
		command string
		path    string
	}{
		{name: "unlisted command", command: "grep -c seed seed.txt"},
		{name: "command with a glob", command: "ls src/*"},
		{name: "command with a quote", command: `grep -c "seed" seed.txt`},
		{name: "compound command", command: "grep -c seed seed.txt && echo done"},
		{name: "unplanned path", path: "other/deep/x.go"},
		{name: "unplanned path with a space", path: "other/my dir/x.go"},
		{name: "unplanned path with a glob char", path: "other/a[bc].go"},
	}

	for _, allowCommands := range []string{"false", "true"} {
		for _, sub := range subjects {
			t.Run(fmt.Sprintf("allow_commands=%s/%s", allowCommands, sub.name), func(t *testing.T) {
				f := newFixtureWith(t, map[string]string{
					flags.HarnessPosture:      flags.PostureStrict,
					flags.GrantsAgentCommands: allowCommands,
				})
				checkout(t, f)

				blocked := probeSubject(t, f, sub.command, sub.path)
				if blocked == nil {
					t.Skip("subject is not blocked in this configuration")
				}
				rule, _ := blocked["rule"].(string)
				target, _ := blocked["target"].(string)

				args := map[string]string{"rule": rule, "reason": "stress: argued for this subject"}
				if sub.command != "" {
					args["command"] = target
				} else {
					args["path"] = target
				}
				out := f.payload("devcouncil_request_override", args)

				if out["granted"] != true {
					// A refusal is a legitimate outcome. It must name the rule
					// that was actually evaluated, and an authority that could
					// clear it — never silence.
					if got, _ := out["rule"].(string); got != "" && got != rule {
						t.Errorf("refused a request about %q by evaluating %q", rule, got)
					}
					if out["suggested_action"] == nil {
						t.Error("a refusal named no authority that could clear it")
					}
					return
				}

				// It said yes. The rule it granted must be the rule that blocked.
				if got, _ := out["rule"].(string); got != rule {
					t.Fatalf("asked about %q, granted %q", rule, got)
				}
				// And the subject must now get through.
				after := probeSubject(t, f, sub.command, sub.path)
				if after != nil {
					t.Fatalf("granted %q on %q, and it is still refused: %v", rule, target, after)
				}
			})
		}
	}
}

// probeSubject evaluates a subject and returns its refusal payload, or nil when
// it is not refused. Only a policy denial counts; a command that runs and exits
// non-zero has not been refused.
func probeSubject(t *testing.T, f *fixture, command, path string) map[string]any {
	t.Helper()
	if command != "" {
		res := f.call("devcouncil_exec_command", map[string]string{"command": command})
		var payload map[string]any
		if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
			return nil
		}
		if payload["action"] != string(policy.Deny) {
			return nil
		}
		return payload
	}
	payload := f.payload("devcouncil_policy_check_write", map[string]string{"path": path})
	if payload["allowed"] == true {
		return nil
	}
	return payload
}

// TestConcurrentOverrideRequestsStayConsistent runs the seam under contention.
// The ledger is shared, the advice consults it, and a grant is issued while
// other calls are reading the same policy.
func TestConcurrentOverrideRequestsStayConsistent(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureStrict,
		flags.GrantsAgentCommands: "true",
	})
	checkout(t, f)

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan string, workers*4)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("other/deep/w%d.go", i)
			command := fmt.Sprintf("grep -c seed%d seed.txt", i)

			blocked := f.payload("devcouncil_policy_check_write", map[string]string{"path": path})
			if blocked["allowed"] == true {
				return
			}
			if blocked["agent_grantable"] != true {
				errs <- fmt.Sprintf("%s: scope.unplanned reported agent_grantable=%v", path, blocked["agent_grantable"])
			}
			out := f.payload("devcouncil_request_override", map[string]string{
				"path": path, "rule": fmt.Sprint(blocked["rule"]), "reason": "concurrent stress",
			})
			if out["granted"] != true {
				errs <- fmt.Sprintf("%s: override refused: %v", path, out)
			}

			cmdRes := f.call("devcouncil_exec_command", map[string]string{"command": command})
			var cmdPayload map[string]any
			if err := json.Unmarshal([]byte(cmdRes.Text), &cmdPayload); err == nil &&
				cmdPayload["action"] == string(policy.Deny) {
				cout := f.payload("devcouncil_request_override", map[string]string{
					"command": command, "rule": fmt.Sprint(cmdPayload["rule"]), "reason": "concurrent stress",
				})
				if cout["granted"] != true {
					errs <- fmt.Sprintf("%s: command override refused: %v", command, cout)
				}
				if cout["scope_persisted"] != nil {
					errs <- fmt.Sprintf("%s: command grant wrote into file scope: %v", command, cout)
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}
