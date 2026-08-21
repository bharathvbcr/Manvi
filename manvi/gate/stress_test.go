package gate

import (
	"sync"
	"testing"
	"time"

	"manvi/dc"
	"manvi/flags"
	"manvi/grants"
	"manvi/policy"
)

// TestDecisionsUnderALiveSettingsChange.
//
// The gate reads its mode flags at the point of use, which is what makes a
// settings change land on the next decision. It also means a decision can be
// taken while the value it is about to read is being written. Run with -race.
//
// The invariant asserted beyond the race detector is the one that matters for
// safety: whatever mode a decision read, the decision it produced must be
// self-consistent — an allow reached under a demoted gate must say which flag
// demoted it. A decision that lost its Demoted field to a concurrent write
// would be an allow that reads as a clean pass.
func TestDecisionsUnderALiveSettingsChange(t *testing.T) {
	g := newGate(t, nil)
	task := testTask()

	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		modes := []string{flags.ModeEnforce, flags.ModeAdvisory, flags.ModeOff}
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if err := g.Flags.Set(flags.Human, flags.PolicyFileMode, modes[i%len(modes)]); err != nil {
				t.Errorf("set: %v", err)
				return
			}
			if err := g.Flags.Set(flags.Human, flags.GrantsAgentEnabled, []string{"true", "false"}[i%2]); err != nil {
				t.Errorf("set: %v", err)
				return
			}
			if err := g.ReloadPolicy(); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
		}
	}()

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				d, err := g.EvaluateWrite("internal/elsewhere.go", task, dc.OpWrite)
				if err != nil {
					t.Errorf("evaluate: %v", err)
					return
				}
				// A rule fired on this path in every mode. What changes is
				// whether it blocks — and an allow that a mode demoted must
				// carry the demotion, or the record says a rule passed that
				// did not.
				if d.Rule == policy.RuleNone {
					t.Errorf("no rule fired on an unplanned path: %+v", d)
					return
				}
				if !d.Blocked() && d.Demoted == "" && d.GrantID == "" {
					t.Errorf("an allow of an unplanned write carries no demotion and no grant: %+v", d)
					return
				}
				g.Ledger.AgentMayGrant(policy.RuleUnplannedScope)
			}
		}()
	}

	time.Sleep(250 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestReloadPolicyUnderConcurrentIssue. The ledger's policy is replaced while
// grants are being issued against it. Neither the replacement nor an issue may
// lose a grant already recorded — the ledger's contract is that nothing is
// removed to tidy up.
func TestReloadPolicyUnderConcurrentIssue(t *testing.T) {
	g := newGate(t, nil)

	// Two groups, deliberately. The reloader runs until the issuers are done,
	// so waiting on one group for both would wait for a goroutine whose exit
	// condition is that wait having returned.
	stop := make(chan struct{})
	var reloader, wg sync.WaitGroup

	reloader.Add(1)
	go func() {
		defer reloader.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := g.ReloadPolicy(); err != nil {
				t.Errorf("reload: %v", err)
				return
			}
		}
	}()

	const issuers, each = 4, 25
	for i := 0; i < issuers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				_, err := g.Ledger.Issue(grants.Request{
					Grantor: grants.Grantor{Authority: grants.Human, ID: "op"},
					Reason:  "stress",
					Scope: grants.Scope{
						TaskID: "TASK-001",
						Rules:  []policy.RuleID{policy.RuleUnplannedScope},
						Paths:  []string{"src/*.go"},
					},
					TTL: time.Hour,
				})
				if err != nil {
					t.Errorf("issue: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)
	reloader.Wait()

	if got := len(g.Ledger.All()); got != issuers*each {
		t.Fatalf("the ledger holds %d grants, want %d — a reload lost some", got, issuers*each)
	}
	// Every id is distinct: a concurrent issue must not hand two grants the
	// same identity, which is what makes a ledger reviewable.
	ids := map[string]bool{}
	for _, gr := range g.Ledger.All() {
		if ids[gr.ID] {
			t.Fatalf("two grants share the id %s", gr.ID)
		}
		ids[gr.ID] = true
	}
}
