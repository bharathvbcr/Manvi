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

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/dc/devmap"
)

// Adversarial tests for b27-lifecycle (23c8e2fc): endRun, needsEnding and the
// index refresh's cancellation.

// fakeDevmapBin writes a devmap stand-in that answers the client's capability
// probe and runs probeBody for `manifest --help` or buildBody for `build`.
// Every body can use $DIR, the directory the test reads its markers from.
func fakeDevmapBin(t *testing.T, probeBody, buildBody string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	if probeBody == "" {
		probeBody = "echo 'usage: devmap manifest --graph-output PATH'; exit 0"
	}
	script := "#!/bin/sh\nDIR='" + dir + "'\ncase \"$*\" in\n" +
		"*manifest*--help*)\n" + probeBody + "\n;;\n" +
		"*build*)\n" + buildBody + "\n;;\n" +
		"esac\nexit 0\n"
	bin = filepath.Join(dir, "devmap")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- an executable test stand-in.
		t.Fatal(err)
	}
	// Whatever the test concluded, end the stand-in's group: it is a process
	// this test started, found by the pid it wrote.
	t.Cleanup(func() {
		if raw, err := os.ReadFile(filepath.Join(dir, "pid")); err == nil { // #nosec G304 -- a path under t.TempDir.
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && pid > 1 {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			}
		}
	})
	return bin, dir
}

func waitFile(t *testing.T, path string, within time.Duration) bool {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

const trapBody = `echo $$ > "$DIR/pid"
trap 'echo TERM > "$DIR/sig"; exit 0' TERM
trap 'echo INT > "$DIR/sig"; exit 0' INT
echo started > "$DIR/started"
sleep 300 &
wait`

// Criterion 1: the build child receives cancellation, not SIGKILL first. The
// real devmap client runs the build; the stand-in traps SIGTERM and SIGINT and
// records which arrived. A SIGKILL leaves no record, and the child is dead.
func TestAdvB27RealBuildChildReceivesACatchableSignalOnCancel(t *testing.T) {
	bin, dir := fakeDevmapBin(t, "", trapBody)
	client := devmap.New(bin, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = refreshIndex(ctx, client)
	}()
	if !waitFile(t, filepath.Join(dir, "started"), 20*time.Second) {
		t.Fatal("the stand-in build never started: the client did not reach it")
	}
	problem := endRun(cancel, done, indexRefreshBound, endLeftoverChildren)
	time.Sleep(700 * time.Millisecond)                 // a trap handler needs milliseconds
	sig, err := os.ReadFile(filepath.Join(dir, "sig")) // #nosec G304 -- a path under t.TempDir.
	if err != nil {
		t.Fatalf("the build child recorded no catchable signal (endRun problem %q): it was SIGKILLed, by the client's cmd.Cancel at cancel() or by the sweep; read error: %v", problem, err)
	}
	if got := strings.TrimSpace(string(sig)); got != "TERM" && got != "INT" {
		t.Fatalf("signal recorded = %q, want TERM or INT", got)
	}
}

// Cancellation alone, with no sweep at all: the client's own cmd.Cancel must not
// be SIGKILL either. Without endRun's sweep the child is still dead within a
// moment of cancel(), and recorded nothing, so the SIGKILL the new ordering was
// meant to remove from the sweep is carried out at cancel() instead.
func TestAdvB27CancelAloneDoesNotSIGKILLTheBuildGroup(t *testing.T) {
	bin, dir := fakeDevmapBin(t, "", trapBody)
	client := devmap.New(bin, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = refreshIndex(ctx, client)
	}()
	if !waitFile(t, filepath.Join(dir, "started"), 20*time.Second) {
		t.Fatal("the stand-in build never started")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pid")) // #nosec G304 -- a path under t.TempDir.
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the refresh did not return after cancel")
	}
	time.Sleep(700 * time.Millisecond)
	_, sigErr := os.Stat(filepath.Join(dir, "sig"))
	if sigErr != nil {
		t.Fatalf("after cancel() alone the build child (pid %d, alive=%v) recorded no signal: the client's cancel is a SIGKILL of the group", pid, zadvAlive(t, pid))
	}
}

// The same, for the capability probe that runs first. The probe goroutine is
// detached from cancellation (context.WithoutCancel), so cancel() reaches it
// not at all, and the sweep is the first signal it gets.
func TestAdvB27RealProbeChildReceivesACatchableSignalOnCancel(t *testing.T) {
	bin, dir := fakeDevmapBin(t, trapBody, "echo unreachable; exit 0")
	client := devmap.New(bin, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = refreshIndex(ctx, client)
	}()
	if !waitFile(t, filepath.Join(dir, "started"), 20*time.Second) {
		t.Fatal("the stand-in probe never started")
	}
	problem := endRun(cancel, done, indexRefreshBound, endLeftoverChildren)
	time.Sleep(700 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(dir, "sig")); err != nil {
		t.Fatalf("the probe child recorded no catchable signal (endRun problem %q): the first signal it received was the sweep's SIGKILL; %v", problem, err)
	}
}

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
	if needsEnding(t.Context(), ps, *row) {
		t.Fatal("needsEnding is true for a zombie that does not lead a group; the sweep would signal a group that is not its own")
	}
	if got := endLeftoverChildren(); got != "" {
		t.Fatalf("a non-leader zombie was reported: %q", got)
	}
}

// A group whose every member is a zombie has nothing to kill: needsEnding reads
// it from one table read, and a single live member flips the answer.
func TestAdvB27NeedsEndingReadsTheWholeGroup(t *testing.T) {
	zadvUsePS(t, `case "$*" in
"-A -o pid=,pgid=,stat=") echo "500 500 Z"; echo "501 500 Z+"; echo "502 777 S";;
esac`)
	ps, _ := trustedPS()
	if needsEnding(t.Context(), ps, childRow{pid: 500, pgid: 500, zombie: true}) {
		t.Fatal("a zombie leader whose group is all zombies (and a live stranger in another group) needs nothing")
	}
	zadvUsePS(t, `case "$*" in
"-A -o pid=,pgid=,stat=") echo "500 500 Z"; echo "501 500 S";;
esac`)
	ps, _ = trustedPS()
	if !needsEnding(t.Context(), ps, childRow{pid: 500, pgid: 500, zombie: true}) {
		t.Fatal("a zombie leader with a live member needs ending")
	}
}

// When ps fails on the allZombies read, the sweep signals the zombie leader's
// group. That group cannot be a stranger's here: the leader is unreaped, so it
// holds the number. The live member dies and the sweep stays bounded.
func TestAdvB27FailedAllZombiesReadStillKillsTheLeadersOwnGroup(t *testing.T) {
	zadvUsePS(t, `[ "$*" = "-A -o pid=,pgid=,stat=" ] && exit 1
exec REAL "$@"`)
	_, member := zombieLeaderWithLiveMember(t)
	start := time.Now()
	_ = endLeftoverChildren()
	if d := time.Since(start); d > childrenReapBound+3*time.Second {
		t.Fatalf("the sweep took %v", d)
	}
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
// signalled even when the allZombies read fails: the re-check of the parent
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
	zadvUsePS(t, `[ "$*" = "-A -o pid=,pgid=,stat=" ] && exit 1
case "$*" in
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
