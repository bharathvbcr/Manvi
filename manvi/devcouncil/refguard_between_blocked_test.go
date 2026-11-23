package devcouncil

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

// A ref that moved between two calls was not moved by the call that reports it:
// the command ran, and nothing was rolled back. Result.Blocked is the gate's
// verdict that it refused this call, and a report of someone else's move is not
// that, so counting it as one tells the operator the gate contained something
// when nothing was contained. The report still marks the result an error and
// carries the rule and the words.
func TestBetweenCallsRefReportDoesNotSetBlocked(t *testing.T) {
	f := shellCommitFixture(t, true)
	if res := shell(f, "echo one"); res.IsError {
		t.Fatalf("a plain call failed: %+v", res)
	}
	run(t, f.root, "commit", "-qm", "leak made between calls")

	res := shell(f, "echo two")
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) ||
		!strings.Contains(res.Text, "ref_changed_between_calls") || !strings.Contains(res.Text, "between agent calls") {
		t.Fatalf("the between-call leak was not reported: %+v", res)
	}
	if res.Blocked {
		t.Fatalf("a report of a ref the command did not move was counted as a gate refusal: %+v", res)
	}
	if !strings.Contains(res.Text, "two") {
		t.Errorf("the command itself did not run: %s", res.Text)
	}
}
