//go:build unix

package codingagent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// pidAlive says whether pid names a live process; a zombie still counts, as it
// does for kill(2).
func pidAlive(pid int) bool { return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) }

// killOnCleanup SIGKILLs pid (a group when negative) once the test ends,
// whatever it concluded. A target already gone is the expected case, not an
// error.
func killOnCleanup(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Errorf("cleanup: kill %d: %v", pid, err)
		}
	})
}

// startStranger starts an unrelated process in a group of its own, standing in
// for whatever takes a group number after the agent's group is gone. It is
// killed and reaped on cleanup.
func startStranger(t *testing.T) *exec.Cmd {
	t.Helper()
	stranger := exec.CommandContext(t.Context(), "/bin/sleep", "30")
	stranger.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := stranger.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Wait reports the SIGKILL the group cleanup below sends (or the
		// context's); reaping is all that is wanted from it.
		_ = stranger.Wait()
	})
	killOnCleanup(t, -stranger.Process.Pid)
	return stranger
}

// startGrandchildAgent starts perl as the provider with script, which receives
// the path where its grandchild writes its pid, and returns the connection and
// that pid once it is known. The grandchild is killed on cleanup whatever the
// test concluded, so a failing assertion never leaves it running.
func startGrandchildAgent(t *testing.T, script string) (*connection, int) {
	t.Helper()
	perl, err := exec.LookPath("perl")
	if err != nil {
		t.Skip("no perl to start a grandchild")
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	c, err := startConnection(t.Context(), perl, []string{"-e", script, pidFile}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The test asserts on its own close(); this one only makes sure the
		// connection is shut, so a failed assertion leaves nothing running.
		_, _ = c.close()
	})
	var pid int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		raw, err := os.ReadFile(pidFile)
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > 0 {
			pid = n
			break
		}
	}
	if pid == 0 {
		t.Fatal("the grandchild never reported its pid")
	}
	killOnCleanup(t, pid)
	return c, pid
}

// A grandchild that ignores SIGTERM and holds the agent's stdout open does not
// survive close(): the agent leads its own process group and cancel kills the
// group while Wait is still pending. This pins that behaviour; it is not the
// case close() used to get wrong (see the test below).
func TestCloseKillsAGrandchildThatIgnoresStdinClosureAndHoldsStdout(t *testing.T) {
	c, grandchild := startGrandchildAgent(t, `
		if (fork() == 0) {
			open(F, ">$ARGV[0]"); print F $$; close F;
			$SIG{TERM} = "IGNORE"; $SIG{HUP} = "IGNORE";
			sleep 30; exit 0;
		}
		sleep 30`)
	exit, err := c.close()
	if !exit.Reaped {
		t.Fatalf("close: exit = %+v, err = %v, want the agent reaped", exit, err)
	}
	for deadline := time.Now().Add(2 * time.Second); pidAlive(grandchild) && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
	}
	if pidAlive(grandchild) {
		t.Fatalf("grandchild %d survived close()", grandchild)
	}
}

// countSignals replaces signalGroup with one that counts and still delivers.
func countSignals(t *testing.T) *atomic.Int32 {
	t.Helper()
	var n atomic.Int32
	old := signalGroup
	signalGroup = func(pid int) { n.Add(1); old(pid) }
	t.Cleanup(func() { signalGroup = old })
	return &n
}

// The shape cmd.Cancel cannot reach: the agent exits, a grandchild in its group
// has redirected stdout and stderr away, so Wait has already returned and there
// is nothing left for cancel to interrupt. close() used to report the agent
// reaped with a nil error while the grandchild, which can still write the
// checkout, ran on. The group is now killed once, as the agent is reaped, and
// close() only waits for it.
func TestCloseKillsAGrandchildThatRedirectedItsStdioAfterTheAgentExited(t *testing.T) {
	signals := countSignals(t)
	c, grandchild := startGrandchildAgent(t, `
		if (fork() == 0) {
			open(STDOUT, ">/dev/null"); open(STDERR, ">/dev/null");
			open(F, ">$ARGV[0]"); print F $$; close F;
			sleep 30; exit 0;
		}
		select(undef, undef, undef, 0.01) until -s $ARGV[0];
		exit 0`)
	<-c.done
	if n := signals.Load(); n != 1 {
		t.Fatalf("the group was signalled %d times as the agent was reaped, want once", n)
	}
	exit, err := c.close()
	if !exit.Reaped || exit.Code != 0 || exit.GroupLive || err != nil {
		t.Fatalf("close: exit = %+v, err = %v, want the agent reaped with code 0 and no error", exit, err)
	}
	if pidAlive(grandchild) {
		t.Fatalf("grandchild %d is still running after close() reported the agent reaped", grandchild)
	}
	if n := signals.Load(); n != 1 {
		t.Fatalf("close() signalled the group (%d signals in all); only the reap-time sweep may", n)
	}
}

// A group found empty when the agent is reaped is never signalled or probed
// again: its number is free, and a later close() must not reach whatever process
// group has taken it. The stranger here is given the agent's group number.
func TestCloseNeverTouchesAGroupThatWasEmptyWhenTheAgentWasReaped(t *testing.T) {
	signals := countSignals(t)
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	stranger := startStranger(t)
	c.pid = stranger.Process.Pid // the number the agent's group had, now held by another group
	exit, err := c.close()
	if !exit.Reaped || exit.GroupLive || err != nil {
		t.Fatalf("close: exit = %+v, err = %v, want a clean confirmation without looking at the number", exit, err)
	}
	if n := signals.Load(); n != 0 {
		t.Fatalf("close() or the sweep signalled %d times with nothing in the group", n)
	}
	if !pidAlive(stranger.Process.Pid) {
		t.Fatal("an unrelated process group was killed")
	}
}

// A group that had a member when the agent was reaped and was empty by the time
// close() ran (the member exited on its own) is the case a reap-time snapshot got
// wrong: close() signalled the number with no fresh look, and by then another
// process group could hold it. Only the sweep, which probes first, signals now.
func TestCloseNeverSignalsAfterTheSweepEvenIfTheNumberIsReusedByAnotherGroup(t *testing.T) {
	signals := countSignals(t)
	realProbe := probeGroup
	var swept atomic.Bool
	probeGroup = func(pid int) bool { // a member exists at the sweep, and only then
		if swept.CompareAndSwap(false, true) {
			return true
		}
		return realProbe(pid)
	}
	oldBudget := reapBudget
	reapBudget = 200 * time.Millisecond
	t.Cleanup(func() { probeGroup, reapBudget = realProbe, oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	stranger := startStranger(t)
	c.pid = stranger.Process.Pid
	// The stranger's group reads as a live member, so close() reports one; what
	// this test is about is that nothing but the sweep signalled.
	_, _ = c.close()
	if n := signals.Load(); n != 1 {
		t.Fatalf("%d signals in all, want only the sweep's", n)
	}
	if !pidAlive(stranger.Process.Pid) {
		t.Fatal("close() killed an unrelated process group that held the number")
	}
}

// Repeated and concurrent close calls agree and do not race (run with -race).
func TestConcurrentCloseOfAnAgentWithAGroupAgrees(t *testing.T) {
	c, _ := startGrandchildAgent(t, `
		if (fork() == 0) {
			open(STDOUT, ">/dev/null"); open(STDERR, ">/dev/null");
			open(F, ">$ARGV[0]"); print F $$; close F;
			sleep 30; exit 0;
		}
		sleep 30`)
	var wg sync.WaitGroup
	exits := make([]Exit, 8)
	errs := make([]error, 8)
	for i := range exits {
		wg.Add(1)
		go func() { defer wg.Done(); exits[i], errs[i] = c.close() }()
	}
	wg.Wait()
	for i := range exits {
		if !exits[i].Reaped || exits[i].GroupLive || errs[i] != nil {
			t.Errorf("close #%d: exit = %+v, err = %v", i, exits[i], errs[i])
		}
	}
}

// A zombie left in the group by a process that has itself left the group (setsid)
// and never waits cannot be cleared by SIGKILL, and kill(2) cannot tell it from a
// running member. The group stays reported live for as long as that parent lives:
// the known limit of group liveness, which fails closed.
func TestAZombieOrphanedInTheGroupByAParentOutsideItIsReportedLive(t *testing.T) {
	oldBudget := reapBudget
	reapBudget = time.Second
	t.Cleanup(func() { reapBudget = oldBudget })
	c, _ := startGrandchildAgent(t, `
		use POSIX;
		if (fork() == 0) {
			if (fork() == 0) { sleep 300; exit 0 }
			POSIX::setsid();
			open(F, ">$ARGV[0]"); print F $$; close F;
			sleep 300; exit 0;
		}
		select(undef, undef, undef, 0.01) until -s $ARGV[0];
		exit 0`)
	<-c.done
	exit, err := c.close()
	if !exit.Reaped || !exit.GroupLive || err == nil {
		t.Fatalf("close: exit = %+v, err = %v, want the zombie-held group reported live", exit, err)
	}
}

// A member that outlives SIGKILL cannot be produced with a real process, so the
// probe is replaced. close() must then say the group is not gone, within the
// budget, and stay able to confirm it on a later call (the reaper in serve
// calls it again) once the member is.
func TestCloseReportsAProcessGroupThatOutlivesSIGKILL(t *testing.T) {
	var alive atomic.Bool
	alive.Store(true)
	oldProbe, oldBudget := probeGroup, reapBudget
	probeGroup, reapBudget = func(int) bool { return alive.Load() }, 150*time.Millisecond
	t.Cleanup(func() { probeGroup, reapBudget = oldProbe, oldBudget })
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", "exit 0"}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	<-c.done
	start := time.Now()
	exit, err := c.close()
	if err == nil || !strings.Contains(err.Error(), "still has a live member") {
		t.Fatalf("close: err = %v, want the live group reported", err)
	}
	if !exit.Reaped || exit.Code != 0 || !exit.GroupLive {
		t.Fatalf("close: exit = %+v, want the direct child reported reaped with code 0 and GroupLive beside the error", exit)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("close took %v, want it bounded by the reap budget", elapsed)
	}
	alive.Store(false)
	if exit, err = c.close(); err != nil || !exit.Reaped || exit.GroupLive {
		t.Fatalf("close once the group is gone: exit = %+v, err = %v, want a clean confirmation", exit, err)
	}
}
