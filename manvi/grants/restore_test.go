package grants

import (
	"strings"
	"testing"
	"time"

	"manvi/policy"
)

// A ledger file is not a trusted input. It outlives the run that wrote it, it
// sits in the repository where anything can edit it, and it is read back as
// grants that clear refusals. Restore therefore has to apply the checks Issue
// applies — and two of them it did not.
//
// The shape of both defects is the same: a record that Issue would refuse to
// mint came back through Restore as a live grant.

func restoreLedger(t *testing.T, p Policy) *Ledger {
	t.Helper()
	l := NewLedger(p)
	l.Now = func() time.Time { return time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC) }
	return l
}

// An agent grant is bounded by the task the agent holds. Issue refuses one that
// names no task; Restore used not to, and the result was a grant covering every
// task and every path in the repository.
func TestRestoreRefusesAnAgentGrantThatNamesNoTask(t *testing.T) {
	l := restoreLedger(t, DefaultPolicy())
	now := l.now()

	unscoped := Grant{
		ID:      "GRANT-0001",
		Grantor: Grantor{Authority: Agent, ID: "executor"},
		Reason:  "restored from a previous run",
		Scope: Scope{
			// No TaskID. Issue refuses exactly this.
			Rules: []policy.RuleID{policy.RuleUnplannedScope},
		},
		IssuedAt:  now.Add(-time.Minute),
		ExpiresAt: now.Add(10 * time.Minute),
	}

	refused := l.Restore([]Grant{unscoped})
	if len(refused) != 1 {
		t.Fatalf("an unscoped agent grant must be refused on restore; refusals=%v", refused)
	}
	if !strings.Contains(refused[0], "names no task") {
		t.Errorf("refusal should say why: %q", refused[0])
	}

	// And it must not clear anything.
	d := policy.Decision{
		Action: policy.Deny, Rule: policy.RuleUnplannedScope,
		Severity: policy.Soft, Target: "any/other/file.go", TaskID: "SOME-OTHER-TASK",
	}
	if _, used := l.Apply(d); used {
		t.Fatal("the refused grant still cleared a decision on an unrelated task")
	}

	// The same grant, correctly scoped, restores — so this is a scoping check
	// rather than a blanket refusal of restored agent grants.
	scoped := unscoped
	scoped.Scope.TaskID = "TASK-001"
	if refused := l.Restore([]Grant{scoped}); len(refused) != 0 {
		t.Fatalf("a task-scoped agent grant should restore; refusals=%v", refused)
	}
}

// The tightest ceiling an operator can set used to be the one that switched the
// expiry check off, because the guard read `ceiling > 0`.
func TestRestoreEnforcesTheCeilingEvenWhenItIsZero(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ceiling time.Duration
	}{
		{"zero ceiling", 0},
		{"negative ceiling is not a longer one", -time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := DefaultPolicy()
			p.AgentMaxTTL = tc.ceiling
			l := restoreLedger(t, p)
			now := l.now()

			forever := Grant{
				ID:        "GRANT-0001",
				Grantor:   Grantor{Authority: Agent, ID: "executor"},
				Reason:    "restored from a previous run",
				Scope:     Scope{TaskID: "TASK-001", Rules: []policy.RuleID{policy.RuleUnplannedScope}},
				IssuedAt:  now.Add(-time.Minute),
				ExpiresAt: now.Add(100 * 365 * 24 * time.Hour),
			}
			refused := l.Restore([]Grant{forever})
			if len(refused) != 1 {
				t.Fatalf("a ceiling of %s can only mint a grant that expires at once; "+
					"a hundred-year record must not restore. refusals=%v", tc.ceiling, refused)
			}
			if !strings.Contains(refused[0], "ceiling") {
				t.Errorf("refusal should name the ceiling: %q", refused[0])
			}
		})
	}
}

// The converse, so the two fixes above are not simply "refuse more": an
// ordinary, well-formed record still restores and still works.
func TestRestoreStillAcceptsAWellFormedGrant(t *testing.T) {
	l := restoreLedger(t, DefaultPolicy())
	now := l.now()

	good := Grant{
		ID:        "GRANT-0007",
		Grantor:   Grantor{Authority: Agent, ID: "executor"},
		Reason:    "widening scope for the test beside the file being fixed",
		Scope:     Scope{TaskID: "TASK-001", Rules: []policy.RuleID{policy.RuleUnplannedScope}, Paths: []string{"src/**"}},
		IssuedAt:  now.Add(-time.Minute),
		ExpiresAt: now.Add(5 * time.Minute),
	}
	if refused := l.Restore([]Grant{good}); len(refused) != 0 {
		t.Fatalf("a well-formed agent grant must restore; refusals=%v", refused)
	}
	d := policy.Decision{
		Action: policy.Deny, Rule: policy.RuleUnplannedScope,
		Severity: policy.Soft, Target: "src/helper.go", TaskID: "TASK-001",
	}
	cleared, used := l.Apply(d)
	if !used {
		t.Fatal("the restored grant should clear the decision it was scoped for")
	}
	if cleared.GrantID != "GRANT-0007" {
		t.Errorf("cleared by %q, want GRANT-0007", cleared.GrantID)
	}
}
