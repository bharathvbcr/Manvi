package agents

import (
	"fmt"
	"testing"
)

func lifecycleInst(t *testing.T, m *InstanceManager, id, state string) *Instance {
	t.Helper()
	inst, err := NewInstance(id, "builder", "worker", func() {})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Register(inst); err != nil {
		t.Fatal(err)
	}
	if state != StateRunning {
		if err := inst.SetState(state, "test"); err != nil {
			t.Fatal(err)
		}
	}
	return inst
}

func TestKillAllIgnoresFinishedChildren(t *testing.T) {
	m := NewInstanceManager()
	lifecycleInst(t, m, "c-done", StateCompleted)
	lifecycleInst(t, m, "c-failed", StateErrored)
	lifecycleInst(t, m, "c-live", StateRunning)
	killed, err := m.KillAll()
	if err != nil || len(killed) != 1 || killed[0] != "c-live" {
		t.Fatalf("killed=%v err=%v; finished children are neither killed nor failures", killed, err)
	}
}

func TestKillAllWithOnlyFinishedChildrenStillErrors(t *testing.T) {
	m := NewInstanceManager()
	lifecycleInst(t, m, "c-done", StateCompleted)
	if killed, err := m.KillAll(); err == nil || len(killed) != 0 {
		t.Fatalf("killed=%v err=%v; nothing live must stay an error", killed, err)
	}
}

func TestKillOnCancelingInstanceIsNotASecondKill(t *testing.T) {
	m := NewInstanceManager()
	lifecycleInst(t, m, "c", StateRunning)
	if err := m.Kill("c"); err != nil {
		t.Fatal(err)
	}
	if err := m.Kill("c"); err == nil {
		t.Fatal("second kill of an already-canceling child reported success")
	}
}

func TestFinishedInstancesAreBounded(t *testing.T) {
	m := NewInstanceManager()
	live := lifecycleInst(t, m, "live", StateRunning)
	for i := 0; i < 4*maxRetainedFinished; i++ {
		lifecycleInst(t, m, fmt.Sprintf("done-%d", i), StateCompleted)
	}
	got := m.Snapshot()
	if len(got) > maxRetainedFinished+2 {
		t.Fatalf("registry holds %d instances after %d dispatches; bound is %d finished + the newest + running",
			len(got), 4*maxRetainedFinished, maxRetainedFinished)
	}
	if _, ok := m.Get(live.ConversationID); !ok {
		t.Fatal("a running instance was pruned")
	}
	if _, ok := m.Get(fmt.Sprintf("done-%d", 4*maxRetainedFinished-1)); !ok {
		t.Fatal("the newest finished instance was pruned before older ones")
	}
}
