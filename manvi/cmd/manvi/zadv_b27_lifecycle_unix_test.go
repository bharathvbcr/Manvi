//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// Adversarial tests for b27-lifecycle (23c8e2fc): endRun and needsEnding. The
// tests that the real devmap client's build and probe children get a catchable
// signal on cancel stay on b27-adv as the acceptance tests for ft-4e2e8926
// (DevCouncil's proc.ConfigureGroup cancel is a group SIGKILL, and Probe runs
// under context.WithoutCancel).

// A refresh that returns just after the bound: the sweep runs exactly once,
// the report names the late refresh, and the quit is not held past the bound.
func TestAdvB27EndRunRefreshJustAfterTheBound(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	const bound = 100 * time.Millisecond
	go func() { time.Sleep(bound + 50*time.Millisecond); close(done) }()
	var sweeps atomic.Int32
	start := time.Now()
	problem := endRun(cancel, done, bound, func() string { sweeps.Add(1); return "" })
	d := time.Since(start)
	if sweeps.Load() != 1 {
		t.Fatalf("sweep ran %d times, want 1", sweeps.Load())
	}
	if !strings.Contains(problem, "did not stop") {
		t.Fatalf("problem = %q, want the late refresh named", problem)
	}
	if d < bound || d > bound+400*time.Millisecond {
		t.Fatalf("endRun took %v, want about %v", d, bound)
	}
}

// A refresh that never returns, with the real sweep: endRun returns within
// bound plus the sweep's own bound, and the sweep still ends a live child.
func TestAdvB27EndRunHungRefreshStillRunsTheRealSweepWithinBounds(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := zadvChild(t, true)
	const bound = 200 * time.Millisecond
	start := time.Now()
	problem := endRun(cancel, make(chan struct{}), bound, endLeftoverChildren)
	if d := time.Since(start); d > bound+childrenReapBound+2*time.Second {
		t.Fatalf("endRun took %v, over bound %v + sweep bound %v", d, bound, childrenReapBound)
	}
	if !strings.Contains(problem, "did not stop") {
		t.Fatalf("problem = %q, want the hung refresh named", problem)
	}
	deadline := time.Now().Add(5 * time.Second)
	for zadvAlive(t, c.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d survived a sweep that ran after a hung refresh", c.Process.Pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cancel is called by endRun and again by the deferred cancel in runTUI: the
// second call must be harmless, and endRun must call it exactly once, even when
// the refresh had already finished.
func TestAdvB27EndRunCancelsOnceAndASecondCancelIsHarmless(t *testing.T) {
	ctx, realCancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	cancel := func() { calls.Add(1); realCancel() }
	done := make(chan struct{})
	close(done)
	if problem := endRun(cancel, done, time.Second, func() string { return "" }); problem != "" {
		t.Fatalf("problem = %q for a refresh that had already returned", problem)
	}
	if calls.Load() != 1 {
		t.Fatalf("endRun called cancel %d times, want 1", calls.Load())
	}
	cancel()
	realCancel()
	if ctx.Err() != context.Canceled {
		t.Fatalf("ctx.Err() = %v, want Canceled", ctx.Err())
	}
}

// A nil refreshDone has nothing to wait for. endRun treats it like a refresh
// that never returns: it waits the whole bound and reports a stop failure that
// did not happen. Unreachable today (runTUI always makes the channel), pinned
// so a caller that passes nil is not surprised. Asserts the observed contract:
// no panic, bounded, sweep still runs.
func TestAdvB27EndRunNilRefreshDoneIsBoundedAndSweeps(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	var swept atomic.Bool
	start := time.Now()
	problem := endRun(cancel, nil, 80*time.Millisecond, func() string { swept.Store(true); return "" })
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("a nil refreshDone held endRun for %v", d)
	}
	if !swept.Load() {
		t.Fatal("the sweep did not run for a nil refreshDone")
	}
	if problem != "" {
		t.Logf("nil refreshDone is reported as a stuck refresh: %q", problem)
	}
}

// waitZombie waits until pid is a zombie.
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st, _ := exec.CommandContext(t.Context(), "/bin/ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- literal.
		if strings.HasPrefix(strings.TrimSpace(string(st)), "Z") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d never became a zombie", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// exitedChild starts a child that exits at once and is never waited for, so it
// is a zombie of this process. leader puts it in a group of its own.
func exitedChild(t *testing.T, leader bool) *exec.Cmd {
	t.Helper()
	c := exec.CommandContext(context.WithoutCancel(t.Context()), "sh", "-c", "exit 0") // #nosec G204 -- literal.
	if leader {
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Wait() })
	waitZombie(t, c.Process.Pid)
	return c
}

// A zombie leader whose group holds nothing but itself needs nothing, and is
// not a problem to report.
func TestAdvB27ZombieLeaderWithNothingLiveIsNotReported(t *testing.T) {
	c := exitedChild(t, true)
	if got := endLeftoverChildren(); got != "" {
		t.Fatalf("a dead leader with an empty group was reported: %q (pid %d)", got, c.Process.Pid)
	}
}

// A zombie that does not lead its group belongs to a group that is not its own
// (here this test process's), and is left alone: not signalled, not reported,
// and needsEnding says so.
func TestAdvB27ZombieThatDoesNotLeadItsGroupIsLeftAlone(t *testing.T) {
	c := exitedChild(t, false)
	ps, ok := trustedPS()
	if !ok {
		t.Skip("no trusted ps")
	}
	rows, err := childRows(t.Context(), ps)
	if err != nil {
		t.Fatal(err)
	}
	var row *childRow
	for i := range rows {
		if rows[i].pid == c.Process.Pid {
			row = &rows[i]
		}
	}
	if row == nil || !row.zombie || row.pgid == row.pid {
		t.Fatalf("setup: row = %+v, want a zombie that does not lead its group", row)
	}
	if needsEnding(*row) {
		t.Fatal("needsEnding is true for a zombie that does not lead a group; the sweep would signal a group that is not its own")
	}
	if got := endLeftoverChildren(); got != "" {
		t.Fatalf("a non-leader zombie was reported: %q", got)
	}
}

// A group whose every member is a zombie has nothing to kill: childRows reads
// that from the one table read, and a single live member flips the answer.
func TestAdvB27NeedsEndingReadsTheWholeGroup(t *testing.T) {
	self := strconv.Itoa(os.Getpid())
	leaderRow := func(table string) childRow {
		t.Helper()
		zadvUsePS(t, "echo '"+table+"'")
		ps, _ := trustedPS()
		rows, err := childRows(t.Context(), ps)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			if r.pid == 500 {
				return r
			}
		}
		t.Fatalf("no row for the leader in %d rows", len(rows))
		return childRow{}
	}
	if needsEnding(leaderRow("500 " + self + " 500 Z\n501 500 500 Z+\n502 1 777 S")) {
		t.Fatal("a zombie leader whose group is all zombies (and a live stranger in another group) needs nothing")
	}
	if !needsEnding(leaderRow("500 " + self + " 500 Z\n501 1 500 S")) {
		t.Fatal("a zombie leader with a live member needs ending")
	}
}

// When the table read gone() makes after the kill fails (the listing the sweep
// started from succeeded, every later one does not), the sweep still signals the
// zombie leader's group, which cannot be a stranger's: the leader is unreaped, so
// it holds the number. The live member dies and the sweep stays bounded. What it
// cannot do is confirm the group gone, and it must say that, not that a child is
// "still running": the child that was killed is not known to be running.
func TestAdvB27FailedAllZombiesReadStillKillsTheLeadersOwnGroup(t *testing.T) {
	count := filepath.Join(t.TempDir(), "table-reads")
	zadvUsePS(t, `if [ "$*" = "-A -o pid=,ppid=,pgid=,stat=" ]; then
  if [ -e '`+count+`' ]; then exit 1; fi
  : > '`+count+`'
fi
exec REAL "$@"`)
	_, member := zombieLeaderWithLiveMember(t)
	start := time.Now()
	got := endLeftoverChildren()
	d := time.Since(start)
	if d > childrenReapBound+3*time.Second {
		t.Fatalf("the sweep took %v", d)
	}
	if _, err := os.Stat(count); err != nil {
		t.Fatalf("the stub never saw the first table read, so the injected failure did not happen: %v", err)
	}
	if strings.Contains(got, "still running") || strings.Contains(got, "could not be ended") {
		t.Fatalf("a killed child whose group could not be re-read was reported as still running: %q", got)
	}
	if !strings.Contains(got, "could not be confirmed gone") {
		t.Fatalf("the sweep did not say the group could not be confirmed gone: %q", got)
	}
	t.Logf("sweep returned %q after %v", got, d)
	deadline := time.Now().Add(5 * time.Second)
	for zadvAlive(t, member) {
		if time.Now().After(deadline) {
			t.Fatalf("member %d of the zombie leader's group survived", member)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A leader that was listed as an unreaped zombie of ours but is already reaped
// when the sweep gets to it (the abandoned goroutine's Wait won the race), whose
// group number is held by a live stranger group, must not get that group
// signalled even when the table read fails: the re-check of the parent
// finds no such pid.
func TestAdvB27ReapedLeaderWhoseGroupNumberIsInUseIsNotSignalled(t *testing.T) {
	leader := exec.CommandContext(context.WithoutCancel(t.Context()), "sh", "-c", "sleep 300 </dev/null >/dev/null 2>&1 & echo $!") // #nosec G204 -- literal.
	leader.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	out, err := leader.Output() // waits: the leader is reaped, its group lives on
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(stranger, syscall.SIGKILL) })
	lp := strconv.Itoa(leader.Process.Pid)
	if err := syscall.Kill(-leader.Process.Pid, 0); err != nil {
		t.Fatalf("setup: the leader's group %s is gone: %v", lp, err)
	}
	self := strconv.Itoa(os.Getpid())
	zadvUsePS(t, `case "$*" in
*-p*) exec REAL "$@";;
esac
echo "`+lp+` `+self+` `+lp+` Z"`)
	_ = endLeftoverChildren()
	if !zadvAlive(t, stranger) {
		t.Fatalf("the live group %s (member %d) was signalled through a reaped leader's number", lp, stranger)
	}
}

// The "unchecked" bookkeeping when the shared bound runs out: a zombie leader
// with nothing live in its group is dead, not unchecked. The listing is read,
// then every later ps hangs until the bound ends; the changed line
// (!rest.zombie || rest.pgid == rest.pid) names the dead leader as a child that
// "could not be checked and was left running".
func TestAdvB27ZombieLeaderIsNotReportedUncheckedWhenTheBoundRunsOut(t *testing.T) {
	zadvUsePS(t, `[ "$*" = "-A -o pid=,ppid=,pgid=,stat=" ] && exec REAL "$@"
sleep 30`)
	c := exitedChild(t, true)
	if got := endLeftoverChildren(); got != "" {
		t.Fatalf("a dead leader with an empty group was reported when the bound ran out: %q (pid %d)", got, c.Process.Pid)
	}
}
