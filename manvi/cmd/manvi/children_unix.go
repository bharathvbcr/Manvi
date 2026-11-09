//go:build unix

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
)

// childrenReapBound is how long ending the process's leftover children may
// take, in all: listing them, and then waiting for the kill to land.
const childrenReapBound = 3 * time.Second

// psCandidates are where the system's ps lives: /bin on macOS, and on Linux
// /bin or, where /bin is a link to it, /usr/bin. Never PATH: a directory the
// agent can write to may be on it, and a ps found there could name any process
// the user owns as ours (the class of ft-6ff33aaf).
var psCandidates = []string{"/bin/ps", "/usr/bin/ps"}

// trustedPS returns the system ps, resolved and judged outside the agent's
// reach, or false when there is none.
func trustedPS() (string, bool) {
	for _, c := range psCandidates {
		if p, ok := trustedResolved(c); ok {
			return p, true
		}
	}
	return "", false
}

// endLeftoverChildren kills every child this process still has and waits,
// bounded, until each is gone. It returns a problem for the caller to report,
// or "" when every child was ended or none was there.
//
// The TUI's index refresh runs a `devmap` that leads its own process group, and
// the client abandons that call as soon as the run's context is cancelled: the
// SIGKILL to the group is sent by a goroutine of the exec package that the
// exiting process does not wait for. Under load the process can be gone first,
// and the build it orphaned went on writing .devcouncil/codeintel
// (ft-90ef3b78). The client does not hand out the pid, so the process table is
// the one place left to find it.
//
// This is a mitigation. The cause is in DevCouncil, where the devmap client's
// invoke abandons the call instead of waiting for the cancel to be carried out
// (ft-4e2e8926: invoke should use RunBoundedWithCleanup, and Probe should run
// with a cancellable context); once that lands this sweep has
// nothing to find, and should be removed.
//
// It is every child, not only devmap's: at this point the run is over, the
// turns have been waited for and the sessions released, so nothing still
// running under this process has an owner that is going to end it. A child that
// leads a group is killed with the group, so what it spawned goes too.
//
// Without a trusted ps there is no sweep, and that is reported; it is never
// replaced by one found on PATH. Each child is checked to still be this
// process's own immediately before it is killed, because one listed a moment
// ago may have exited and been reaped, and its pid handed to a stranger. What
// is left is the gap between that check and the kill itself: two system calls,
// microseconds, against a pid that would have to be reused in that time.
func endLeftoverChildren() string {
	ps, ok := trustedPS()
	if !ok {
		return "no trusted ps was found, so children that outlived the run were not looked for"
	}
	ctx, cancel := context.WithTimeout(context.Background(), childrenReapBound)
	defer cancel()
	rows, err := childRows(ctx, ps)
	if err != nil {
		return "the process table could not be read, so children that outlived the run were not looked for: " + err.Error()
	}
	self := os.Getpid()
	var live []childRow
	for _, r := range rows {
		if r.zombie {
			continue // dead already; only its parent's Wait is missing
		}
		// Re-read now: the listing is stale, and a pid is only ours while its
		// parent is.
		if ppid, err := parentOf(ctx, ps, r.pid); err != nil || ppid != self {
			continue
		}
		if r.pgid == r.pid {
			proc.KillGroup(r.pid)
		} else {
			_ = syscall.Kill(r.pid, syscall.SIGKILL)
		}
		live = append(live, r)
	}
	var left []int
	for _, r := range live {
		if !gone(ctx, r) {
			left = append(left, r.pid)
		}
	}
	if len(left) > 0 {
		return fmt.Sprintf("child processes %v were still running at exit and could not be ended", left)
	}
	return ""
}

// gone waits, until ctx ends, for the child to be reaped. Its parent's Wait is
// what removes it and that runs in the goroutine that abandoned it, so a short
// poll is what there is to wait on.
func gone(ctx context.Context, r childRow) bool {
	target := r.pid
	if r.pgid == r.pid {
		target = -r.pid // a group is gone when its last member is
	}
	for {
		if err := syscall.Kill(target, 0); err == syscall.ESRCH {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type childRow struct {
	pid, pgid int
	zombie    bool
}

// runPS runs the trusted ps with args, bounded like every other exec boundary:
// a fork that never returns must not hold the exit. It returns what ps wrote
// and its own pid, which is a child of this process while it runs and must not
// be mistaken for one that outlived the run.
func runPS(ctx context.Context, ps string, args ...string) (string, int, error) {
	// #nosec G204 -- the system ps by absolute path, with literal arguments.
	cmd := exec.CommandContext(ctx, ps, args...)
	proc.ConfigureGroup(cmd)
	cmd.WaitDelay = time.Second
	var out strings.Builder
	cmd.Stdout = &out
	runErr, timedOut := proc.RunBounded(ctx, cmd.Run)
	if timedOut {
		return "", 0, ctx.Err()
	}
	if runErr != nil {
		return "", 0, runErr
	}
	return out.String(), cmd.Process.Pid, nil
}

// childRows lists this process's direct children, bar the ps that listed them.
func childRows(ctx context.Context, ps string) ([]childRow, error) {
	out, psPid, err := runPS(ctx, ps, "-A", "-o", "pid=,ppid=,pgid=,stat=")
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	var rows []childRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil || ppid != self || pid == psPid {
			continue
		}
		rows = append(rows, childRow{pid: pid, pgid: pgid, zombie: strings.HasPrefix(f[3], "Z")})
	}
	return rows, nil
}

// parentOf reads one process's parent. A process that is gone has none, and is
// an error.
func parentOf(ctx context.Context, ps string, pid int) (int, error) {
	out, _, err := runPS(ctx, ps, "-o", "ppid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}
