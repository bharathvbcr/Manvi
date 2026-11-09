//go:build unix

package main

import (
	"context"
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

// endLeftoverChildren kills every child this process still has and waits,
// bounded, until each is gone. It returns the pids still present when the bound
// ran out; nil means none were.
//
// The TUI's index refresh runs a `devmap` that leads its own process group, and
// the client abandons that call as soon as the run's context is cancelled: the
// SIGKILL to the group is sent by a goroutine of the exec package that the
// exiting process does not wait for. Under load the process can be gone first,
// and the build it orphaned went on writing .devcouncil/codeintel
// (ft-90ef3b78). The client does not hand out the pid, so the process table is
// the one place left to find it.
//
// It is every child, not only devmap's: at this point the run is over, the
// turns have been waited for and the sessions released, so nothing still
// running under this process has an owner that is going to end it. A child that
// leads a group is killed with the group, so what it spawned goes too.
func endLeftoverChildren() []int {
	ctx, cancel := context.WithTimeout(context.Background(), childrenReapBound)
	defer cancel()
	rows, err := childRows(ctx)
	if err != nil {
		// The table could not be read, which is not the same as "no children";
		// but exit has to go on, and there is nothing more this can try.
		return nil
	}
	var live []childRow
	for _, r := range rows {
		if r.zombie {
			continue // dead already; only its parent's Wait is missing
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
	return left
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

// childRows lists this process's direct children from `ps`, which reads the
// same on Linux and macOS where /proc is not there to read. The run is bounded
// like every other exec boundary: a fork that never returns must not hold the
// exit.
func childRows(ctx context.Context) ([]childRow, error) {
	// #nosec G204 -- a literal command with literal arguments.
	cmd := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=,ppid=,pgid=,stat=")
	proc.ConfigureGroup(cmd)
	cmd.WaitDelay = time.Second
	var out strings.Builder
	cmd.Stdout = &out
	runErr, timedOut := proc.RunBounded(ctx, cmd.Run)
	if timedOut {
		return nil, ctx.Err()
	}
	if runErr != nil {
		return nil, runErr
	}
	self := os.Getpid()
	var rows []childRow
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		pgid, e3 := strconv.Atoi(f[2])
		if e1 != nil || e2 != nil || e3 != nil || ppid != self {
			continue
		}
		rows = append(rows, childRow{pid: pid, pgid: pgid, zombie: strings.HasPrefix(f[3], "Z")})
	}
	return rows, nil
}
