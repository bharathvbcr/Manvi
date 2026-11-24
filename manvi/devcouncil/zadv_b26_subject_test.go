package devcouncil

import (
	"context"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
)

func advB26PlannedChange(t *testing.T, f *fixture, path string) (dc.AllowedChange, bool) {
	t.Helper()
	task, err := f.reg.deps.Store.Task(context.Background(), "TASK-001")
	if err != nil || task == nil {
		t.Fatalf("task: %v %v", task, err)
	}
	for _, pf := range task.PlannedFiles {
		if pf.Path == path {
			return pf.AllowedChange, true
		}
	}
	return "", false
}

// The block the model hit was scope.unplanned. Naming scope.operation instead is
// the one lever left over the operation that gets persisted: it must not turn a
// modify grant into one that also covers deletes.
func TestAdvB26SubjectModelNamedRuleCannotWidenToDelete(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	got := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.operation", "reason": "a new file",
	})
	if got["granted"] != true {
		t.Skipf("override refused, so there is nothing to widen: %v", got)
	}
	if got["scope_persisted"] != true {
		t.Skipf("nothing persisted: %v", got)
	}
	change, found := advB26PlannedChange(t, f, "internal/helper.go")
	if !found {
		t.Fatalf("no planned entry: %v", got)
	}
	if change == dc.ChangeDelete {
		t.Fatalf("the model named rule scope.operation on an unplanned path it was blocked on with scope.unplanned, and the persisted scope now permits deleting it: %v", got)
	}
}

func TestAdvB26SubjectPathAndReasonWordsNeverPersistDelete(t *testing.T) {
	cases := []struct{ name, path, reason string }{
		{"path-name", "internal/delete_helper.go", "a new file"},
		{"path-dir", "internal/delete/helper.go", "a new file"},
		{"reason-upper", "internal/a.go", "DELETE nothing; this is NOT a Delete"},
		{"reason-delete-word", "internal/b.go", "delete"},
	}
	for _, c := range cases {
		f := newFixture(t)
		f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
		got := f.payload("devcouncil_request_override", map[string]string{
			"path": c.path, "rule": "scope.unplanned", "reason": c.reason,
		})
		if got["granted"] != true || got["scope_persisted"] != true {
			t.Errorf("%s: setup: %v", c.name, got)
			continue
		}
		if change, found := advB26PlannedChange(t, f, c.path); !found || change == dc.ChangeDelete {
			t.Errorf("%s: persisted change = %q found=%v", c.name, change, found)
		}
	}
}

// With no rule at all the operation is a write regardless of the reason.
func TestAdvB26SubjectNoRuleReasonMentioningDeleteStaysModify(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})
	got := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/c.go", "reason": "please delete the old one and write this",
	})
	if got["granted"] != true {
		t.Skipf("not granted: %v", got)
	}
	if got["scope_persisted"] == true {
		if change, _ := advB26PlannedChange(t, f, "internal/c.go"); change == dc.ChangeDelete {
			t.Fatalf("reason text persisted a delete: %v", got)
		}
	}
}

// Two sub-agents, two owner labels, two tasks: distinct leases, each released by
// its own session, and not by the other's.
func TestAdvB26SubjectTwoChildrenHoldDistinctLeasesAndReleaseOwn(t *testing.T) {
	f := newFixture(t)
	insertTask(t, f.db, "TASK-002", `[{"path":"src/other.go","allowed_change":"modify"}]`)
	a := f.reg.RootSession().NewChildSession(nil)
	b := f.reg.RootSession().NewChildSession(nil)
	ra := f.callAs(a, "devcouncil_checkout_task", map[string]any{"task_id": "TASK-001", "owner": "worker-a"})
	rb := f.callAs(b, "devcouncil_checkout_task", map[string]any{"task_id": "TASK-002", "owner": "worker-b"})
	if ra.IsError || rb.IsError {
		t.Fatalf("checkout failed: %q %q", ra.Text, rb.Text)
	}
	if a.State().TaskID != "TASK-001" || b.State().TaskID != "TASK-002" || a.State().Token == b.State().Token {
		t.Fatalf("leases are not distinct: %+v %+v", a.State(), b.State())
	}
	la, _ := f.reg.deps.Store.Active(context.Background(), "TASK-001")
	lb, _ := f.reg.deps.Store.Active(context.Background(), "TASK-002")
	if la == nil || lb == nil || la.Owner != "agent:worker-a" || lb.Owner != "agent:worker-b" {
		t.Fatalf("lease owner labels lost: %+v %+v", la, lb)
	}
	if res := f.callAs(a, "devcouncil_release_task", map[string]any{}); res.IsError {
		t.Fatalf("a could not release its own lease: %q", res.Text)
	}
	if l, _ := f.reg.deps.Store.Active(context.Background(), "TASK-002"); l == nil {
		t.Fatal("a's release took b's lease")
	}
	if res := f.callAs(b, "devcouncil_release_task", map[string]any{}); res.IsError {
		t.Fatalf("b could not release its own lease: %q", res.Text)
	}
}

// Children keep the harness identity, so both are recorded as the same grantor.
// The ledger can no longer tell one sub-agent's override from another's.
func TestAdvB26SubjectChildGrantorsAreDistinguishable(t *testing.T) {
	f := newFixture(t)
	insertTask(t, f.db, "TASK-002", `[{"path":"src/other.go","allowed_change":"modify"}]`)
	a := f.reg.RootSession().NewChildSession(nil)
	b := f.reg.RootSession().NewChildSession(nil)
	f.callAs(a, "devcouncil_checkout_task", map[string]any{"task_id": "TASK-001", "owner": "worker-a"})
	f.callAs(b, "devcouncil_checkout_task", map[string]any{"task_id": "TASK-002", "owner": "worker-b"})
	if a.State().Owner == b.State().Owner {
		t.Fatalf("two sub-agents share grantor identity %q: an override by one cannot be told from the other's in the ledger", a.State().Owner)
	}
}

// An override still works for a session whose lease label differs from its
// grantor identity (anything that cross-checks them would refuse here).
func TestAdvB26SubjectOverrideWorksWithCustomLeaseLabel(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001", "owner": "worker-x"})
	got := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/d.go", "rule": "scope.unplanned", "reason": "a new file",
	})
	if got["granted"] != true {
		t.Fatalf("override refused after a labelled checkout: %v", got)
	}
}

// The lease label is still whatever the model typed. It is shown by
// manvi lease and the watch face as who holds the task.
func TestAdvB26SubjectLeaseLabelCannotImpersonateHuman(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001", "owner": "human:operator"})
	l, _ := f.reg.deps.Store.Active(context.Background(), "TASK-001")
	if l == nil {
		t.Fatal("no lease")
	}
	if l.Owner == "human:operator" {
		t.Fatalf("the lease is recorded under the model-chosen label %q, and watch/lease faces print it as the holder", l.Owner)
	}
}
