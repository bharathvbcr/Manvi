//go:build unix

package codingagent

import (
	"context"
	"errors"
	"os/exec"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Adversarial tests for the process-group reaping of ft-43e7cb92 (w8-procgroup).

// Root cause of the pre-existing flake in
// TestCloseNeverSignalsAfterTheSweepEvenIfTheNumberIsReusedByAnotherGroup
// ("cleanup: kill -N: operation not permitted"): startStranger builds the
// stranger with exec.CommandContext(t.Context()), and t.Context() is cancelled
// before the cleanups run, so os/exec SIGKILLs the stranger on its own. The
// stranger is then a zombie (nobody has Waited; that is the next cleanup). On
// Darwin kill(2) to a process group whose only member is a zombie answers EPERM,
// not ESRCH and not success, and killOnCleanup reports anything but ESRCH as a
// test error. Whether the cleanup's kill lands before or after the exec kill is
// a race, hence about one run in fifty. This pins the syscall behaviour that
// makes it so, with no race: a zombie-only group is EPERM on Darwin.
func TestAZombieOnlyGroupAnswersKillWithEPERMOnDarwin(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("EPERM for a zombie-only group is Darwin's answer; Linux signals a zombie successfully")
	}
	zombie := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sleep", "30")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	pid := zombie.Process.Pid
	t.Cleanup(func() { _ = zombie.Wait() })
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	// Wait until the process is a zombie: it still exists for kill(pid, 0) but
	// cannot be signalled again. Not Waited, so it stays one.
	deadline := time.Now().Add(5 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = syscall.Kill(-pid, syscall.SIGKILL); err != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Fatalf("kill(-%d, SIGKILL) on a zombie-only group = %v, want EPERM (the cause of the cleanup flake)", pid, err)
	}
	// And the product reads that as a live group, by design (fail closed): a
	// zombie-only group never reads as gone while its parent has not reaped it.
	if !groupAlive(pid) {
		t.Fatalf("groupAlive(%d) = false for a zombie-only group", pid)
	}
}

// The reap-time sweep found a member (it was the only moment anything was
// signalled), the member then left on its own, and an unrelated process group
// took the number. close() probes first, so it never signals the stranger (the
// existing test pins that). What the existing test discards is close()'s answer:
// the stranger's member reads as the agent's, so close() reports GroupLive
// and a plain run's hold, which used to be a harness-only concern, now stays on
// the checkout for as long as a stranger's process lives. The group number is
// the agent's pid and was free once the agent was reaped, so this needs a pid
// cycle or a deliberate setpgid(0, victimPid) by an unrelated process.
func TestAStrangersGroupHoldingTheNumberIsNotReportedAsTheAgentsLiveGroup(t *testing.T) {
	realProbe := probeGroup
	var swept atomic.Bool
	probeGroup = func(pid int) bool {
		if swept.CompareAndSwap(false, true) {
			return true // a member existed at the sweep, which is what makes the group unlatched
		}
		return realProbe(pid)
	}
	oldBudget := reapBudget
	reapBudget = 150 * time.Millisecond
	t.Cleanup(func() { probeGroup, reapBudget = realProbe, oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	stranger := startStranger(t)
	c.pid = stranger.Process.Pid
	exit, _ := c.close()
	if exit.GroupLive {
		t.Fatalf("close() reported GroupLive because a stranger's process group holds the number: exit = %+v; a plain run's checkout is now held for as long as that stranger lives", exit)
	}
}

// A Wait that never returns ends close() after one budget, with the group
// unmentioned (Reaped false), and every later close() call is again exactly one
// budget: the reaper in serve costs one budget per attempt, never more, and a
// call that did time out leaves nothing running that a later call would race.
func TestCloseOfAnAgentWhoseWaitNeverReturnsCostsOneBudgetEachCall(t *testing.T) {
	oldBudget := reapBudget
	reapBudget = 100 * time.Millisecond
	t.Cleanup(func() { reapBudget = oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	c.done = make(chan struct{}) // Wait as close() sees it: never
	for i := range 3 {
		start := time.Now()
		exit, err := c.close()
		took := time.Since(start)
		if err == nil || exit.Reaped || exit.GroupLive {
			t.Fatalf("call %d: exit = %+v, err = %v, want not reaped with an error", i, exit, err)
		}
		if took < 90*time.Millisecond || took > 400*time.Millisecond {
			t.Fatalf("call %d took %v, want one budget (100ms)", i, took)
		}
	}
}

// When the members cannot be listed at the sweep, nothing proves the group
// that later holds the number is not the agent's, so close() fails closed and
// reports it live, exactly as the bare probe did before the listing existed.
func TestCloseReportsTheGroupLiveWhenTheSweepCouldNotListItsMembers(t *testing.T) {
	realProbe, realSnapshot := probeGroup, snapshotGroup
	var swept atomic.Bool
	probeGroup = func(pid int) bool {
		if swept.CompareAndSwap(false, true) {
			return true
		}
		return realProbe(pid)
	}
	snapshotGroup = func(int) ([]groupMember, error) { return nil, errors.New("sysctl refused") }
	oldBudget := reapBudget
	reapBudget = 150 * time.Millisecond
	t.Cleanup(func() { probeGroup, snapshotGroup, reapBudget = realProbe, realSnapshot, oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	stranger := startStranger(t)
	c.pid = stranger.Process.Pid
	exit, _ := c.close()
	if !exit.GroupLive {
		t.Fatalf("close() = %+v, want GroupLive when the sweep's member listing failed", exit)
	}
}

// The second of the sweep's two listings failing alone is as unknowable as the
// first failing: close() reports the group live.
func TestCloseReportsTheGroupLiveWhenOnlyTheSecondListingFails(t *testing.T) {
	realProbe, realSnapshot := probeGroup, snapshotGroup
	var swept atomic.Bool
	probeGroup = func(pid int) bool {
		if swept.CompareAndSwap(false, true) {
			return true
		}
		return realProbe(pid)
	}
	var lists atomic.Int32
	snapshotGroup = func(int) ([]groupMember, error) {
		if lists.Add(1) == 2 {
			return nil, errors.New("sysctl refused")
		}
		return nil, nil
	}
	oldBudget := reapBudget
	reapBudget = 150 * time.Millisecond
	t.Cleanup(func() { probeGroup, snapshotGroup, reapBudget = realProbe, realSnapshot, oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	stranger := startStranger(t)
	c.pid = stranger.Process.Pid
	exit, _ := c.close()
	if !exit.GroupLive {
		t.Fatalf("close() = %+v, want GroupLive when the second listing failed", exit)
	}
	if lists.Load() != 2 {
		t.Fatalf("listings = %d, want 2", lists.Load())
	}
}
