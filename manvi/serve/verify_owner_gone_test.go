//go:build linux || darwin

package serve

import (
	"context"
	"encoding/json"
	"testing"
)

// Where a process's start time can be read (codingagent.ProcessStart), the
// record of an owner that has been killed and reaped is settled on the next
// read from another process, and the settlement names why. The platforms that
// cannot read it settle on age alone (TestOwnerEndedNamesItsBasis).
func TestAGoneOwnersRunningRecordIsSettledNamingWhy(t *testing.T) {
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	h.change()
	owner := startOwnerProcess(t, h)

	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait() // reaped, so its pid names no process: the kill's own exit status is not a test result
	settled, err := h.runner.Get(context.Background(), json.RawMessage(`{"item_id":"t","request_id":"req-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if settled.Record.State != verifySettled || settled.Record.Outcome != VerifyDegraded {
		t.Fatalf("a record whose owner is gone = %+v, want it settled as degraded", settled.Record)
	}
	requireDegraded(t, settled.Record, "no longer exists")
	if h.critic.calls.Load() != 0 {
		t.Fatal("settling an orphan ran the critic")
	}
}
