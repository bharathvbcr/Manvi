package grants

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"manvi/policy"
)

func ledger(t *testing.T) *Ledger {
	t.Helper()
	return NewLedger(DefaultPolicy())
}

func denial(rule policy.RuleID, target, task string) policy.Decision {
	return policy.Decision{
		Action: policy.Deny, Rule: rule, Severity: policy.SeverityOf(rule),
		Target: target, TaskID: task, Reason: "test denial",
	}
}

// TestGrantForALiteralPathDoesNotCoverGlobSiblings is a scope-widening attack.
// SuggestRequest copies the decision's target into Scope.Paths, where it is
// interpreted as an fnmatch *pattern*. A real file whose name contains glob
// metacharacters therefore yields a grant broader than the block it cleared.
func TestGrantForALiteralPathDoesNotCoverGlobSiblings(t *testing.T) {
	l := ledger(t)
	blocked := denial(policy.RuleUnplannedScope, "src/a[bc].go", "TASK-1")
	req, err := SuggestRequest(blocked, Grantor{Authority: Human, ID: "op"})
	if err != nil {
		t.Fatal(err)
	}
	req.Reason = "needed for the fix"
	req.Scope.Once = false
	if _, err := l.Issue(req); err != nil {
		t.Fatal(err)
	}

	// The grant was issued for one literal path. A different real file must not
	// be covered by it.
	sibling := denial(policy.RuleUnplannedScope, "src/ab.go", "TASK-1")
	if _, used := l.Apply(sibling); used {
		t.Fatal("a grant for src/a[bc].go must not clear a block on src/ab.go")
	}

	// ...and the path it was actually issued for must still be cleared.
	if _, used := l.Apply(blocked); !used {
		t.Fatal("the grant must still clear the exact path it was issued for")
	}
}

// TestConcurrentApplyConsumesASingleUseGrantOnce is the race that would let two
// parallel writes share one single-use agent grant.
func TestConcurrentApplyConsumesASingleUseGrantOnce(t *testing.T) {
	l := ledger(t)
	_, err := l.Issue(Request{
		Grantor: Grantor{Authority: Agent, ID: "agent-1"},
		Reason:  "unplanned helper",
		Scope: Scope{
			TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleUnplannedScope},
			Paths: []string{"src/*.go"}, Once: true,
		},
		TTL: time.Minute, AgentTask: "TASK-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	const workers = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	cleared := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d := denial(policy.RuleUnplannedScope, fmt.Sprintf("src/f%d.go", i), "TASK-1")
			if _, used := l.Apply(d); used {
				mu.Lock()
				cleared++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if cleared != 1 {
		t.Fatalf("a single-use grant cleared %d writes, want exactly 1", cleared)
	}
}

// TestExpiredGrantNeverApplies pins the TTL boundary rather than trusting it.
func TestExpiredGrantNeverApplies(t *testing.T) {
	l := ledger(t)
	now := time.Now()
	l.Now = func() time.Time { return now }
	g, err := l.Issue(Request{
		Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "temporary",
		Scope: Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleUnplannedScope}, Paths: []string{"src/a.go"}},
		TTL:   time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	d := denial(policy.RuleUnplannedScope, "src/a.go", "TASK-1")
	if _, used := l.Apply(d); !used {
		t.Fatal("grant should apply while live")
	}
	// Exactly at expiry it must already be dead: Active uses Before, so the
	// boundary instant is closed. Pin it so a later refactor cannot open it.
	now = g.ExpiresAt
	if _, used := l.Apply(d); used {
		t.Fatal("a grant must not apply at its own expiry instant")
	}
}

// TestAgentCannotEscapeItsTaskByRenamingTheScope walks the ways an agent grant
// could be widened past its own lease.
func TestAgentCannotEscapeItsTaskByRenamingTheScope(t *testing.T) {
	l := ledger(t)
	agent := Grantor{Authority: Agent, ID: "agent-1"}
	cases := []struct {
		name string
		req  Request
	}{
		{"another task", Request{Grantor: agent, Reason: "r", AgentTask: "TASK-1",
			Scope: Scope{TaskID: "TASK-2", Rules: []policy.RuleID{policy.RuleUnplannedScope}}}},
		{"no task at all", Request{Grantor: agent, Reason: "r", AgentTask: "TASK-1",
			Scope: Scope{Rules: []policy.RuleID{policy.RuleUnplannedScope}}}},
		{"a hard rule", Request{Grantor: agent, Reason: "r", AgentTask: "TASK-1",
			Scope: Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleSecretPath}}}},
		{"a non-agent-grantable soft rule", Request{Grantor: agent, Reason: "r", AgentTask: "TASK-1",
			Scope: Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleReadOnly}}}},
		{"an unknown rule", Request{Grantor: agent, Reason: "r", AgentTask: "TASK-1",
			Scope: Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleID("rule.invented")}}}},
		{"a forged authority", Request{Grantor: Grantor{Authority: "root", ID: "x"}, Reason: "r",
			Scope: Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleUnplannedScope}}}},
		{"an empty rule set", Request{Grantor: agent, Reason: "r", AgentTask: "TASK-1",
			Scope: Scope{TaskID: "TASK-1"}}},
	}
	for _, tc := range cases {
		if _, err := l.Issue(tc.req); err == nil {
			t.Errorf("%s: Issue succeeded, want a refusal", tc.name)
		}
	}
}

// TestRestoreCannotResurrectASpentGrant: reloading persisted grants must not
// hand back a single-use grant that was already spent, nor extend an expiry.
func TestRestoreCannotResurrectASpentGrant(t *testing.T) {
	l := ledger(t)
	spent := Grant{
		ID: "GRANT-0001", Grantor: Grantor{Authority: Agent, ID: "a"}, Reason: "old",
		Scope:    Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleUnplannedScope}, Paths: []string{"src/a.go"}, Once: true},
		IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour), Consumed: true,
	}
	expired := spent
	expired.ID = "GRANT-0002"
	expired.Consumed = false
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	l.Restore([]Grant{spent, expired})

	if _, used := l.Apply(denial(policy.RuleUnplannedScope, "src/a.go", "TASK-1")); used {
		t.Fatal("a restored spent/expired grant must not clear anything")
	}
	// A new grant must not collide with a restored ID.
	fresh, err := l.Issue(Request{Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "new",
		Scope: Scope{Rules: []policy.RuleID{policy.RuleUnplannedScope}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []Grant{spent, expired} {
		if fresh.ID == g.ID {
			t.Fatalf("new grant reused restored ID %q", fresh.ID)
		}
	}
}

// TestConcurrentIssueProducesUniqueIDs: IDs are how a grant is referenced in
// evidence, so a collision would misattribute an override.
func TestConcurrentIssueProducesUniqueIDs(t *testing.T) {
	l := ledger(t)
	const workers = 100
	ids := make(chan string, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := l.Issue(Request{Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "r",
				Scope: Scope{Rules: []policy.RuleID{policy.RuleUnplannedScope}}})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- g.ID
		}()
	}
	wg.Wait()
	close(ids)
	seen := map[string]bool{}
	for id := range ids {
		if seen[id] {
			t.Fatalf("duplicate grant ID %q", id)
		}
		seen[id] = true
	}
	if len(seen) != workers {
		t.Fatalf("got %d unique IDs, want %d", len(seen), workers)
	}
}

// TestNegativeTTLDoesNotBecomeTheCeiling: a caller passing a negative lifetime
// most likely computed it, and silently promoting that to the maximum is the
// wrong direction to round.
func TestNegativeTTLDoesNotBecomeTheCeiling(t *testing.T) {
	l := ledger(t)
	g, err := l.Issue(Request{Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "r",
		Scope: Scope{Rules: []policy.RuleID{policy.RuleUnplannedScope}}, TTL: -time.Hour})
	if err == nil {
		t.Fatalf("a negative TTL was accepted and became %s", g.ExpiresAt.Sub(g.IssuedAt))
	}
}

// TestRestoreRefusesWhatIssueWouldRefuse pins re-validation of the durable
// ledger: Restore used to append whatever the file held, so a corrupted or
// hand-edited record could name no rules (matching everything), carry a hard
// rule, or hold an expiry no issue path could mint.
func TestRestoreRefusesWhatIssueWouldRefuse(t *testing.T) {
	l := ledger(t)
	now := time.Now()
	saved := []Grant{
		{
			ID: "GRANT-9001", Grantor: Grantor{Authority: Human, ID: "op"},
			Scope:    Scope{Rules: nil}, // rule-less: matches everything soft
			IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		},
		{
			ID: "GRANT-9002", Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "nope",
			Scope:    Scope{Rules: []policy.RuleID{policy.RuleSecretPath}},
			IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour),
		},
		{
			ID: "GRANT-9003", Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "eternal",
			Scope:    Scope{Rules: []policy.RuleID{policy.RuleUnplannedScope}, Paths: []string{"src/a.go"}},
			IssuedAt: now.Add(-time.Hour), ExpiresAt: now.Add(100 * 24 * 365 * time.Hour),
		},
	}
	refused := l.Restore(saved)
	if len(refused) != len(saved) {
		t.Fatalf("expected all %d records refused, got %v", len(saved), refused)
	}

	if _, used := l.Apply(denial(policy.RuleUnplannedScope, "src/a.go", "TASK-1")); used {
		t.Fatal("a tampered ledger cleared a denial after restore")
	}
	// The valid path still works end to end.
	fresh, err := l.Issue(Request{
		Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "legit",
		Scope: Scope{Rules: []policy.RuleID{policy.RuleUnplannedScope}, Paths: []string{"src/b.go"}},
	})
	if err != nil {
		t.Fatalf("a legitimate issue must still work: %v", err)
	}
	if _, used := l.Apply(denial(policy.RuleUnplannedScope, "src/b.go", "TASK-1")); !used || fresh.ID == "" {
		t.Fatal("the ledger stopped working for honest grants")
	}
}

// TestRestoreKeepsAWellFormedGrant: validation must not become a wipe. A
// grant Issue itself could have minted restores intact and still applies.
func TestRestoreKeepsAWellFormedGrant(t *testing.T) {
	l := ledger(t)
	good := Grant{
		ID: "GRANT-0007", Grantor: Grantor{Authority: Human, ID: "op"}, Reason: "reviewed",
		Scope:    Scope{TaskID: "TASK-1", Rules: []policy.RuleID{policy.RuleUnplannedScope}, Paths: []string{"src/keep.go"}},
		IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour),
	}
	if refused := l.Restore([]Grant{good}); len(refused) != 0 {
		t.Fatalf("an honest record was refused: %v", refused)
	}
	d := denial(policy.RuleUnplannedScope, "src/keep.go", "TASK-1")
	cleared, used := l.Apply(d)
	if !used || cleared.Action != policy.Allow {
		t.Fatalf("the restored grant did not apply: used=%v decision=%+v", used, cleared)
	}
}
