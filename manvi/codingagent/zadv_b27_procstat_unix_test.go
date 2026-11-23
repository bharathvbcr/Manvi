//go:build unix

package codingagent

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// Adversarial tests for b27-lifecycle's listStatError and its consequence for
// close() when every listing fails.

// Classification survives wrapping: the errno is found through fmt.Errorf %w
// and a PathError inside one, the errno is preserved in what is returned, and
// the message names the pid.
func TestAdvB27ListStatErrorClassifiesWrappedErrors(t *testing.T) {
	path := func(e error) error { return &fs.PathError{Op: "open", Path: "/proc/9/stat", Err: e} }
	cases := []struct {
		name     string
		err      error
		wantSkip bool
		is       error
	}{
		{"bare ENOENT", syscall.ENOENT, true, nil},
		{"PathError wrapping ENOENT, wrapped again", fmt.Errorf("read: %w", path(syscall.ENOENT)), true, nil},
		{"PathError wrapping ESRCH, wrapped again", fmt.Errorf("read: %w", path(syscall.ESRCH)), true, nil},
		{"PathError wrapping EACCES, wrapped again", fmt.Errorf("read: %w", path(syscall.EACCES)), false, syscall.EACCES},
		{"PathError wrapping EPERM, wrapped again", fmt.Errorf("read: %w", path(syscall.EPERM)), false, syscall.EPERM},
		{"PathError wrapping EIO, wrapped again", fmt.Errorf("read: %w", path(syscall.EIO)), false, syscall.EIO},
		{"PathError wrapping EINTR", path(syscall.EINTR), false, syscall.EINTR},
		{"PathError wrapping ENOMEM", path(syscall.ENOMEM), false, syscall.ENOMEM},
	}
	for _, tc := range cases {
		got := listStatError(9, tc.err)
		if tc.wantSkip != (got == nil) {
			t.Errorf("%s: listStatError = %v, want skip=%v", tc.name, got, tc.wantSkip)
			continue
		}
		if got == nil {
			continue
		}
		if !errors.Is(got, tc.is) {
			t.Errorf("%s: %v does not unwrap to %v", tc.name, got, tc.is)
		}
		var pe *fs.PathError
		if !errors.As(got, &pe) || pe.Path != "/proc/9/stat" {
			t.Errorf("%s: the PathError is lost from %v", tc.name, got)
		}
	}
}

// What a hidepid=1 host does to close(): every listing fails (the /proc scan
// meets another user's pid). A group that the SIGKILL empties is still reported
// gone, at once: the bare probe answers ESRCH, so the failed listing costs no
// reapBudget in the common case.
func TestAdvB27EveryListingFailingStillReportsAKilledGroupGoneAtOnce(t *testing.T) {
	stubSnapshot(t, func(pgid int) ([]groupMember, error) {
		return nil, fmt.Errorf("read /proc/1/stat while listing a process group: %w", &fs.PathError{Op: "open", Path: "/proc/1/stat", Err: syscall.EACCES})
	})
	shortBudget(t, 2*time.Second)
	c, gc := startGrandchildAgent(t, `
		my $g = fork();
		if ($g == 0) { open(F, ">$ARGV[0]"); print F $$; close F; $SIG{TERM} = 'IGNORE'; sleep 300; exit 0 }
		select(undef, undef, undef, 0.01) until -s $ARGV[0];
		exit 0`)
	start := time.Now()
	exit, err := c.close()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("close: %v", err)
	}
	if exit.GroupLive {
		t.Fatalf("close = %+v: a group SIGKILL emptied was reported live because its listing failed", exit)
	}
	if d := time.Since(start); d >= 2*time.Second {
		t.Fatalf("close took %v, the whole reapBudget, for a group that was gone", d)
	}
	for deadline := time.Now().Add(5 * time.Second); pidAlive(gc) && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
	}
	if pidAlive(gc) {
		t.Fatalf("grandchild %d survived", gc)
	}
}

// And what it costs when the group is NOT emptied (the signal is stubbed away so
// a member survives): the listing never having worked, groupLive stays on the
// bare probe, the group reads live, and awaitGroup returns an error after one
// reapBudget; fail-closed and bounded, not hung.
func TestAdvB27EveryListingFailingCostsOneReapBudgetForASurvivingMember(t *testing.T) {
	stubSnapshot(t, func(pgid int) ([]groupMember, error) {
		return nil, &fs.PathError{Op: "open", Path: "/proc/1/stat", Err: syscall.EACCES}
	})
	oldSignal := signalGroup
	signalGroup = func(int) {}
	t.Cleanup(func() { signalGroup = oldSignal })
	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), "/bin/sleep", "300") // #nosec G204 -- literal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = cmd.Wait() })
	c := &connection{pid: pid}
	c.sweepGroup()
	if c.membersKnown {
		t.Fatal("membersKnown is true although both listings failed")
	}
	if !c.groupLive() {
		t.Fatal("groupLive is false for a group with a live member and no listing")
	}
	const budget = 300 * time.Millisecond
	start := time.Now()
	err := c.awaitGroup(time.After(budget))
	d := time.Since(start)
	if err == nil {
		t.Fatal("awaitGroup returned nil for a group that still has a live member")
	}
	if d < budget || d > budget+2*time.Second {
		t.Fatalf("awaitGroup took %v, want about one budget (%v)", d, budget)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = cmd.Wait()
	if c.groupLive() {
		t.Fatal("groupLive is still true after the member was killed and reaped")
	}
}
