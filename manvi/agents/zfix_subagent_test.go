package agents

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// The tests in this file pin one property of the sub-agent control plane: it
// must never report an outcome it did not achieve. Each one fails against the
// code as it stood.

// zfixLive builds a registered, killable instance and returns it with a
// function reporting whether its cancellation was delivered.
func zfixLive(t *testing.T, m *InstanceManager, id string) (*Instance, func() bool) {
	t.Helper()
	var mu sync.Mutex
	cancelled := false
	inst, err := NewInstance(id, "builder", "worker", func() {
		mu.Lock()
		defer mu.Unlock()
		cancelled = true
	})
	if err != nil {
		t.Fatalf("building instance %q: %v", id, err)
	}
	if err := m.Register(inst); err != nil {
		t.Fatalf("registering instance %q: %v", id, err)
	}
	return inst, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return cancelled
	}
}

// Defect 1. Instance.cancel was never assigned outside a test, so Kill found
// nil, cancelled nothing, and returned nil — which every caller reads as a
// child that has been stopped.
func TestZfixAnInstanceCannotBeBuiltOrRegisteredWithoutACancellationHandle(t *testing.T) {
	if _, err := NewInstance("conv-1", "builder", "worker", nil); err == nil {
		t.Fatal("an instance with no cancellation handle was accepted; nothing could ever terminate it")
	}
	if _, err := NewInstance("  ", "builder", "worker", func() {}); err == nil {
		t.Fatal("an instance with no conversation ID was accepted; nothing could ever name it")
	}

	m := NewInstanceManager()
	// The bare literal is exactly the shape the dispatcher used to register.
	if err := m.Register(&Instance{ConversationID: "conv-2", State: StateRunning}); err == nil {
		t.Fatal("an unkillable instance was registered; it would be listed as a live child and never terminated")
	}
	if _, listed := m.Get("conv-2"); listed {
		t.Fatal("the refused instance is still visible to the control plane")
	}
}

// Defect 1: the cancellation is delivered. Defect 3: every case in which
// nothing was terminated is an error rather than a silent success.
func TestZfixKillCancelsAndSaysSoWhenItCannot(t *testing.T) {
	m := NewInstanceManager()
	ctx, cancel := context.WithCancel(context.Background())
	inst, err := NewInstance("conv-live", "builder", "worker", cancel)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	if err := m.Register(inst); err != nil {
		t.Fatalf("Register: %v", err)
	}

	if err := m.Kill("conv-live"); err != nil {
		t.Fatalf("killing a live instance: %v", err)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("Kill returned success without cancelling the child's context")
	}
	if got := inst.Snapshot().State; got != StateCanceling {
		t.Fatalf("state after kill = %q, want %q", got, StateCanceling)
	}

	// An ID nobody registered.
	err = m.Kill("conv-does-not-exist")
	if err == nil {
		t.Fatal("killing an unregistered ID reported success; nothing was terminated")
	}
	if !strings.Contains(err.Error(), "nothing was terminated") {
		t.Errorf("the refusal does not say nothing happened: %v", err)
	}

	// A child that already finished.
	done, _ := zfixLive(t, m, "conv-done")
	if err := done.SetState(StateCompleted, "done"); err != nil {
		t.Fatalf("completing an instance: %v", err)
	}
	if err := m.Kill("conv-done"); err == nil {
		t.Fatal("killing a completed child reported success")
	}
	if got := done.Snapshot().State; got != StateCompleted {
		t.Fatalf("a completed child was moved back out of a terminal state, to %q", got)
	}
}

// Defect 3: KillAll over an empty manager answered nil, and the caller reported
// "all subagents terminating" for children that did not exist.
func TestZfixKillAllNamesWhatItTerminatedAndRefusesWhenThereIsNothing(t *testing.T) {
	m := NewInstanceManager()
	if killed, err := m.KillAll(); err == nil {
		t.Fatalf("KillAll over an empty manager reported success, killed=%v", killed)
	}

	_, aCancelled := zfixLive(t, m, "conv-a")
	_, bCancelled := zfixLive(t, m, "conv-b")

	killed, err := m.KillAll()
	if err != nil {
		t.Fatalf("KillAll: %v", err)
	}
	if strings.Join(killed, ",") != "conv-a,conv-b" {
		t.Fatalf("KillAll named %v, want both instances", killed)
	}
	if !aCancelled() || !bCancelled() {
		t.Fatalf("KillAll reported %v without cancelling both children", killed)
	}

	// Everything already terminal is not a successful kill_all either.
	settled := NewInstanceManager()
	inst, _ := zfixLive(t, settled, "conv-c")
	if err := inst.SetState(StateErrored, "boom"); err != nil {
		t.Fatalf("erroring an instance: %v", err)
	}
	if killed, err := settled.KillAll(); err == nil {
		t.Fatalf("KillAll with nothing live reported success, killed=%v", killed)
	}
}

// Defect 8. Without transition rules a cancelled child reached a terminal state
// by reporting completion, and a finished child could be moved back out of one.
func TestZfixLifecycleTransitionsAreEnforced(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		allowed  bool
	}{
		{StateRunning, StateCanceling, true},
		{StateRunning, StateCompleted, true},
		{StateRunning, StateErrored, true},
		{StateCanceling, StateErrored, true},
		{StateCanceling, StateCompleted, false},
		{StateCanceling, StateRunning, false},
		{StateCompleted, StateCanceling, false},
		{StateCompleted, StateRunning, false},
		{StateErrored, StateRunning, false},
		{StateErrored, StateCompleted, false},
		{"", StateRunning, false},
	} {
		if got := canTransition(tc.from, tc.to); got != tc.allowed {
			t.Errorf("canTransition(%q, %q) = %v, want %v", tc.from, tc.to, got, tc.allowed)
		}
	}

	m := NewInstanceManager()
	inst, _ := zfixLive(t, m, "conv-1")
	if err := m.Kill("conv-1"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	// The move that matters: a terminated child must not be able to report that
	// it finished its work.
	if err := inst.SetState(StateCompleted, "done"); err == nil {
		t.Fatal("a cancelled child was allowed to report completion")
	}
	if err := inst.SetState(StateErrored, "cancelled"); err != nil {
		t.Fatalf("a cancelled child could not reach a terminal state: %v", err)
	}
	if err := inst.SetState(StateRunning, "back again"); err == nil {
		t.Fatal("a terminal instance was moved back to running")
	}
}

// Defect 7. State and StateDetail are written by the goroutine running the
// child and were read, with no lock, by the control plane rendering a listing.
// Run this under -race.
func TestZfixSnapshotIsSafeWhileStateIsBeingWritten(t *testing.T) {
	m := NewInstanceManager()
	const children = 4
	for i := 0; i < children; i++ {
		zfixLive(t, m, string(rune('a'+i)))
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for _, snap := range m.Snapshot() {
		inst, ok := m.Get(snap.ConversationID)
		if !ok {
			t.Fatalf("instance %q vanished", snap.ConversationID)
		}
		wg.Add(1)
		go func(inst *Instance) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Running -> running is a no-op move, which is the point: it
				// still writes StateDetail under the lock.
				_ = inst.SetState(StateRunning, "still working")
			}
		}(inst)
	}

	for i := 0; i < 2000; i++ {
		if got := len(m.Snapshot()); got != children {
			t.Fatalf("Snapshot returned %d instances, want %d", got, children)
		}
	}
	close(stop)
	wg.Wait()
}

// Defect 6, the half this package owns: the distinction between a role the
// harness shipped and one written at runtime has to be recoverable, because a
// role authored under a shipped name is otherwise indistinguishable from the
// role it replaced.
func TestZfixShippedRolesAreDistinguishableFromAuthoredOnes(t *testing.T) {
	r := NewRegistry()
	for _, name := range []string{"research", "builder", "critic", "planner", "stress_tester", "self"} {
		if !r.IsBuiltIn(name) {
			t.Errorf("%q is a shipped role but is not marked as one", name)
		}
	}
	if r.IsBuiltIn("auditor") {
		t.Error("a role nobody registered is reported as shipped")
	}
	if err := r.Register(Definition{Name: "auditor", SystemPrompt: "audit"}); err != nil {
		t.Fatalf("registering an authored role: %v", err)
	}
	if r.IsBuiltIn("auditor") {
		t.Error("a role authored at runtime is reported as shipped")
	}
	if !r.IsBuiltIn("critic") {
		t.Error("registering an unrelated role lost the shipped marking on critic")
	}
}

// Defect 2, the half this package owns. The inbox has no reader, so the most it
// can honestly report is that a message was queued — and not even that once the
// child is finished.
func TestZfixSendMessageRefusesAChildThatHasFinished(t *testing.T) {
	m := NewInstanceManager()
	inst, _ := zfixLive(t, m, "conv-1")
	if err := m.SendMessage("conv-1", "focus on calc_test.go"); err != nil {
		t.Fatalf("queueing a message for a running child: %v", err)
	}
	if err := inst.SetState(StateCompleted, "done"); err != nil {
		t.Fatalf("completing the instance: %v", err)
	}
	if err := m.SendMessage("conv-1", "too late"); err == nil {
		t.Fatal("a message was queued for a child that had already finished")
	}
}
