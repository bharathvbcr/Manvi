package devcouncil

import (
	"context"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc"
)

// The operation an override grants comes from the rule the block named, never
// from the words of the reason. The scope an override persists takes its
// permission from it, so a reason that merely mentioned deleting — here to say
// it is not one — must not turn a modify into a delete.
func TestOverrideReasonTextNeverChangesTheOperationGranted(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001"})

	granted := f.payload("devcouncil_request_override", map[string]string{
		"path": "internal/helper.go", "rule": "scope.unplanned",
		"reason": "not a delete, a new file",
	})
	if granted["granted"] != true || granted["scope_persisted"] != true {
		t.Fatalf("setup: the override was not granted and persisted: %v", granted)
	}

	task, err := f.reg.deps.Store.Task(context.Background(), "TASK-001")
	if err != nil || task == nil {
		t.Fatalf("task: %v %v", task, err)
	}
	var found bool
	for _, pf := range task.PlannedFiles {
		if pf.Path != "internal/helper.go" {
			continue
		}
		found = true
		if pf.AllowedChange == dc.ChangeDelete {
			t.Fatalf("a reason that mentioned deleting persisted a delete permission: %+v", pf)
		}
	}
	if !found {
		t.Fatalf("the widened path was not persisted: %+v", task.PlannedFiles)
	}
}

// The identity recorded as the grantor of an agent's own overrides is the
// harness's, not a string the model chose when it checked a task out.
func TestCheckoutOwnerArgumentDoesNotChooseTheGrantorIdentity(t *testing.T) {
	f := newFixture(t)
	f.payload("devcouncil_checkout_task", map[string]string{"task_id": "TASK-001", "owner": "evil_owner"})
	if got := f.reg.RootSession().State().Owner; got != "manvi" {
		t.Fatalf("the session identity is %q; it must come from the harness, not the checkout argument", got)
	}
}
