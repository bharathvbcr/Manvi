//go:build unix

package codingagent

import (
	"context"
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

// Adversarial tests for ft-978de814 (w18-procgroup): the member snapshot that
// tells the agent's group from a stranger holding the same number.

// waitPidFile reads a pid a script wrote, within a bound.
func waitPidFile(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		raw, err := os.ReadFile(path) // #nosec G304 -- a path this test made under t.TempDir
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > 0 {
			return n
		}
	}
	t.Fatalf("no pid in %s", path)
	return 0
}

// stubSnapshot swaps the member listing for the test.
func stubSnapshot(t *testing.T, f func(int) ([]groupMember, error)) {
	t.Helper()
	old := snapshotGroup
	snapshotGroup = f
	t.Cleanup(func() { snapshotGroup = old })
}

// shortBudget shortens reapBudget for the test.
func shortBudget(t *testing.T, d time.Duration) {
	t.Helper()
	old := reapBudget
	reapBudget = d
	t.Cleanup(func() { reapBudget = old })
}

// A member forked BETWEEN the sweep's two listings, by a listed member that then
// exits, is in the group but not in the first listing. The second listing names
// it, so the group reads live once the listed member is gone; a single
// pre-signal listing read it free, where w17-int2's bare probe read it live. The
// signal stub stands for the kill window: it releases the listed member M, which
// forks the sleeper B, and returns only once B's pid file exists (bounded), so B
// exists before the second listing and not before the first. It kills nothing,
// so B outlives the sweep, which a real SIGKILL would not allow for a process
// already in the group; what is pinned is the listing, not the kill.
func TestAdvW18ProcgroupMemberForkedBetweenListingsKeepsTheGroupLive(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "go")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dir, "late.pid")
	swept := make(chan struct{})
	oldSignal := signalGroup
	signalGroup = func(int) {
		defer close(swept)
		f, err := os.OpenFile(fifo, os.O_WRONLY, 0) // #nosec G304 -- a path this test made under t.TempDir
		if err != nil {
			return
		}
		_, _ = f.WriteString("go\n")
		_ = f.Close()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
			if raw, err := os.ReadFile(pidFile); err == nil && strings.TrimSpace(string(raw)) != "" { // #nosec G304 -- a path this test made under t.TempDir
				return
			}
		}
	}
	t.Cleanup(func() { signalGroup = oldSignal })
	shortBudget(t, 300*time.Millisecond)
	// The agent leaves one member M behind (an inner sh waiting on the fifo) and
	// exits. M, once released, forks a sleeper and exits.
	script := `sh -c 'read x < ` + fifo + `; sleep 60 >/dev/null 2>&1 & echo $! > ` + pidFile + `' >/dev/null 2>&1 </dev/null & exit 0`
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", script}, t.TempDir(), func([]byte) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-swept:
	case <-time.After(10 * time.Second):
		t.Fatal("sweep never signalled")
	}
	late := waitPidFile(t, pidFile)
	killOnCleanup(t, late)
	// Wait for M to be gone: the late sleeper is then the group's only member.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if m, err := groupMembers(c.pid); err == nil && len(m) == 1 && m[0].pid == late {
			break
		}
	}
	if !pidAlive(late) || !groupAlive(c.pid) {
		t.Fatalf("setup: late member alive=%v, kill(-pgid,0) says alive=%v", pidAlive(late), groupAlive(c.pid))
	}
	exit, _ := c.close()
	if !exit.GroupLive {
		t.Fatalf("close() = %+v: a live process (pid %d) was forked into the agent's group between the sweep's listings but is not named; w17-int2 reported this live", exit, late)
	}
}

// exec() keeps the pid and the start time, so a member that execs after the
// snapshot still matches its snapshot entry. (If it did not, the group would
// read free while the member lives.)
func TestAdvW18ProcgroupExecKeepsTheMemberStartTime(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "go")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sh", "-c", "read x < "+fifo+"; exec /bin/sleep 30") // #nosec G204 -- fixed programs, arguments from this test
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })
	killOnCleanup(t, -pid)
	before, err := groupMembers(pid)
	if err != nil || len(before) != 1 {
		t.Fatalf("before exec: %+v, %v", before, err)
	}
	f, err := os.OpenFile(fifo, os.O_WRONLY, 0) // #nosec G304 -- a path this test made under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("go\n")
	_ = f.Close()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		out, _ := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output() // #nosec G204 -- fixed programs, arguments from this test
		if strings.Contains(string(out), "sleep") {
			break
		}
	}
	after, err := groupMembers(pid)
	if err != nil || len(after) != 1 || after[0] != before[0] {
		t.Fatalf("after exec: %+v, %v; want %+v", after, err, before)
	}
}

// A zombie member (its parent has not reaped it) is part of the listing, as it
// is for kill(2); otherwise a zombie-held group would read free.
func TestAdvW18ProcgroupZombieMemberIsListed(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The parent forks a child that exits at once and never waits for it.
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/usr/bin/perl", "-e", `my $p = fork(); if ($p == 0) { exit 0 } open(F, ">$ARGV[0]"); print F $p; close F; sleep 60`, pidFile) // #nosec G204 -- fixed programs, arguments from this test
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Wait() })
	killOnCleanup(t, cmd.Process.Pid)
	child := waitPidFile(t, pidFile)
	var members []groupMember
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		members, _ = groupMembers(cmd.Process.Pid)
		if len(members) == 2 {
			break
		}
	}
	seen := map[int]bool{}
	for _, m := range members {
		seen[m.pid] = true
	}
	if !seen[cmd.Process.Pid] || !seen[child] {
		t.Fatalf("members = %+v, want the parent %d and its zombie child %d", members, cmd.Process.Pid, child)
	}
}

// groupLive over a stubbed listing: every way a snapshotted member can differ
// from what is listed later.
func TestAdvW18ProcgroupGroupLiveDecisionTable(t *testing.T) {
	stranger := startStranger(t)
	pid := stranger.Process.Pid
	realMembers, err := groupMembers(pid)
	if err != nil || len(realMembers) != 1 {
		t.Fatal(realMembers, err)
	}
	m := realMembers[0]
	cases := []struct {
		name   string
		was    []groupMember
		known  bool
		now    []groupMember
		nowErr error
		want   bool
	}{
		{"same member still there", []groupMember{m}, true, []groupMember{m}, nil, true},
		{"same pid, different start (pid reuse)", []groupMember{{m.pid, m.start - 1}}, true, []groupMember{m}, nil, false},
		{"member gone, stranger present", []groupMember{{m.pid + 1, 5}}, true, []groupMember{m}, nil, false},
		{"empty snapshot means the number was free", nil, true, []groupMember{m}, nil, false},
		{"snapshot unknown fails closed", nil, false, []groupMember{{1, 1}}, nil, true},
		{"later listing error fails closed", []groupMember{m}, true, nil, errors.New("boom"), true},
		{"later listing empty while the probe answers", []groupMember{m}, true, nil, nil, false},
		{"one of several snapshotted members remains", []groupMember{{9, 9}, m, {10, 10}}, true, []groupMember{m}, nil, true},
	}
	for _, tc := range cases {
		stubSnapshot(t, func(int) ([]groupMember, error) { return tc.now, tc.nowErr })
		c := &connection{pid: pid, members: tc.was, membersKnown: tc.known}
		if got := c.groupLive(); got != tc.want {
			t.Errorf("%s: groupLive = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// close() with no sweep that ever listed (members never set) on a group that
// answers: nothing is known, so it must be live, not "free".
func TestAdvW18ProcgroupCloseWithoutAListingFailsClosed(t *testing.T) {
	stranger := startStranger(t)
	c := &connection{pid: stranger.Process.Pid}
	// membersKnown zero value is false: groupLive must read the live group live.
	if !c.groupLive() {
		t.Fatal("groupLive on a connection that never listed its members read a live group as free")
	}
}

// The sweep that finds the probe empty must not list (the number was free), and
// a sweep that finds a member lists exactly twice, around the one signal.
func TestAdvW18ProcgroupSweepListsOnlyWhenTheProbeSeesAMember(t *testing.T) {
	var lists atomic.Int32
	stubSnapshot(t, func(int) ([]groupMember, error) { lists.Add(1); return nil, nil })
	oldProbe, oldSignal := probeGroup, signalGroup
	var signals atomic.Int32
	signalGroup = func(int) { signals.Add(1) }
	t.Cleanup(func() { probeGroup, signalGroup = oldProbe, oldSignal })
	probeGroup = func(int) bool { return false }
	c := &connection{pid: 4242}
	c.sweepGroup()
	if lists.Load() != 0 || signals.Load() != 0 || !c.groupGone.Load() {
		t.Fatalf("empty probe: lists=%d signals=%d gone=%v", lists.Load(), signals.Load(), c.groupGone.Load())
	}
	probeGroup = func(int) bool { return true }
	c = &connection{pid: 4242}
	c.sweepGroup()
	if lists.Load() != 2 || signals.Load() != 1 || !c.membersKnown {
		t.Fatalf("member probe: lists=%d signals=%d known=%v", lists.Load(), signals.Load(), c.membersKnown)
	}
}

// Fail-open hunt: the listing succeeds but is empty although the probe said a
// member exists (sysctl size 0, or the member left in between). Reading that as
// "number free" is the design; pin that a real zombie-only group is NOT empty
// in the listing, which is what makes the design safe on Darwin.
func TestAdvW18ProcgroupZombieOnlyGroupListsNonEmpty(t *testing.T) {
	zombie := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sleep", "30")
	zombie.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := zombie.Start(); err != nil {
		t.Fatal(err)
	}
	pid := zombie.Process.Pid
	t.Cleanup(func() { _ = zombie.Wait() })
	_ = syscall.Kill(pid, syscall.SIGKILL)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if errors.Is(syscall.Kill(-pid, syscall.SIGKILL), syscall.EPERM) {
			break
		}
	}
	members, err := groupMembers(pid)
	if err != nil || len(members) != 1 || members[0].pid != pid {
		t.Fatalf("zombie-only group listing = %+v, %v; the probe says it is live, so an empty listing would read it free", members, err)
	}
}

// Concurrent close() calls and the reaper's repeat calls, on a connection with
// a real lingering member: -race must stay clean and every call must answer.
func TestAdvW18ProcgroupConcurrentCloseIsRaceFree(t *testing.T) {
	c, _ := startGrandchildAgent(t, `
		my $g = fork();
		if ($g == 0) { open(F, ">$ARGV[0]"); print F $$; close F; $SIG{TERM} = 'IGNORE'; sleep 300; exit 0 }
		select(undef, undef, undef, 0.01) until -s $ARGV[0];
		exit 0`)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				_, _ = c.close()
			}
		}()
	}
	wg.Wait()
	// err may carry the pre-existing WaitDelay error (the grandchild held stdout);
	// only the group verdict is under test.
	if exit, err := c.close(); exit.GroupLive {
		t.Fatalf("final close = %+v, %v; the group was SIGKILLed at the sweep", exit, err)
	}
}

// A real group: the agent leaves a grandchild that ignores SIGTERM; the sweep
// SIGKILLs it and close() reports the group gone, as w17-int2 did.
func TestAdvW18ProcgroupRealGrandchildIsKilledAndReportedGone(t *testing.T) {
	c, gc := startGrandchildAgent(t, `
		my $g = fork();
		if ($g == 0) { open(F, ">$ARGV[0]"); print F $$; close F; $SIG{TERM} = 'IGNORE'; sleep 300; exit 0 }
		select(undef, undef, undef, 0.01) until -s $ARGV[0];
		exit 0`)
	exit, err := c.close()
	// The grandchild holds the agent's stdout, so os/exec may report that its
	// WaitDelay expired before the pipe closed. That is the pipe behaviour, not
	// what is under test (the group verdict); any other error is a failure.
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("close: unexpected error %v (exit = %+v)", err, exit)
	}
	if !exit.Reaped || exit.GroupLive {
		t.Fatalf("close = %+v, %v; want the agent reaped and its group gone", exit, err)
	}
	for deadline := time.Now().Add(5 * time.Second); pidAlive(gc) && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	if pidAlive(gc) {
		t.Fatalf("grandchild %d survived", gc)
	}
}

// Cost: groupLive lists the group every groupPoll while it answers. With ~50
// members, log the per-listing time and bound it well under the poll.
func TestAdvW18ProcgroupListingCostWithAFiftyMemberGroup(t *testing.T) {
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sh", "-c", `i=0; while [ $i -lt 50 ]; do sleep 60 & i=$((i+1)); done; wait`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Wait() })
	killOnCleanup(t, -pid)
	var members []groupMember
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		members, _ = groupMembers(pid)
		if len(members) >= 51 {
			break
		}
	}
	if len(members) < 51 {
		t.Fatalf("only %d members listed", len(members))
	}
	const n = 200
	best, total := time.Hour, time.Duration(0)
	for i := 0; i < n; i++ {
		start := time.Now()
		if _, err := groupMembers(pid); err != nil {
			t.Fatal(err)
		}
		d := time.Since(start)
		total += d
		best = min(best, d)
	}
	t.Logf("groupMembers with %d members: min %v mean %v over %d (poll is %v)", len(members), best, total/n, n, groupPoll)
	if total/n > groupPoll/5 {
		t.Fatalf("mean listing %v is over a fifth of the %v poll", total/n, groupPoll)
	}
}
