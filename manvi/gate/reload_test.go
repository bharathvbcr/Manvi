package gate

import (
	"testing"
	"time"

	"manvi/flags"
	"manvi/grants"
	"manvi/policy"
)

// TestGrantPolicyIsStaleUntilReloaded is the divergence this seam closes.
//
// Every other flag the gate consults is read at the point of use, so moving one
// lands on the next decision. The six grants.* flags are copied into the
// ledger's policy when the gate is built. Once a setting can be moved at
// runtime, that copy becomes a second answer to "are agent grants enabled" —
// and the flag table reports the registry's answer, not the ledger's. An
// operator switching agent grants off would be told it was off while the ledger
// went on issuing them.
func TestGrantPolicyIsStaleUntilReloaded(t *testing.T) {
	g := newGate(t, nil)
	if !g.Ledger.AgentMayGrant(policy.RuleUnplannedScope) {
		t.Fatal("agent grants should start enabled; the fixture asserts nothing otherwise")
	}

	if err := g.Flags.Set(flags.Human, flags.GrantsAgentEnabled, "false"); err != nil {
		t.Fatalf("set: %v", err)
	}
	// Stated rather than assumed: the staleness is the reason ReloadPolicy
	// exists, and a future gate that read the flag live would make the reload
	// unnecessary — which this line would catch.
	if !g.Ledger.AgentMayGrant(policy.RuleUnplannedScope) {
		t.Skip("the ledger now reads the registry live; ReloadPolicy is no longer load-bearing here")
	}

	if err := g.ReloadPolicy(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if g.Ledger.AgentMayGrant(policy.RuleUnplannedScope) {
		t.Fatal("the ledger still permits agent grants after the flag was switched off and reloaded")
	}
}

// TestReloadPolicyKeepsIssuedGrants. A grant was argued for and recorded under
// the policy in force at the time; discarding it because a setting moved would
// erase the record rather than tighten it.
func TestReloadPolicyKeepsIssuedGrants(t *testing.T) {
	g := newGate(t, nil)
	before, err := g.Ledger.Issue(grants.Request{
		Grantor: grants.Grantor{Authority: grants.Human, ID: "op"},
		Reason:  "recorded under the old policy",
		Scope: grants.Scope{
			TaskID: "TASK-001",
			Rules:  []policy.RuleID{policy.RuleUnplannedScope},
			Paths:  []string{"src/calc.go"},
		},
		TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	if err := g.Flags.Set(flags.Human, flags.GrantsEnabled, "false"); err != nil {
		t.Fatalf("set: %v", err)
	}
	if err := g.ReloadPolicy(); err != nil {
		t.Fatalf("reload: %v", err)
	}

	var found bool
	for _, gr := range g.Ledger.All() {
		if gr.ID == before.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("grant %s vanished when the policy was reloaded", before.ID)
	}
}

// TestReloadPolicyRefusesAnUnreadableRegistry. A reload that could not read its
// settings must fail rather than install a default — installing DefaultPolicy
// on error is exactly how a gate comes to be more permissive than the file
// that configures it.
func TestReloadPolicyRefusesAnUnreadableRegistry(t *testing.T) {
	g := newGate(t, nil)
	// A registry with no definitions at all cannot answer any of the six keys.
	g.Flags = flags.New()
	if err := g.ReloadPolicy(); err == nil {
		t.Fatal("a reload that could not read grants.enabled reported success")
	}
	// The ledger keeps the policy it had, which is the stricter of the two
	// outcomes available when the new one cannot be computed.
	if !g.Ledger.AgentMayGrant(policy.RuleUnplannedScope) {
		t.Fatal("a failed reload changed the policy anyway")
	}
}
