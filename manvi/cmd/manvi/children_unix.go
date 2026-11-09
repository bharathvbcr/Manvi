//go:build unix

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
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
// is left is the gap between that check and the kill itself: milliseconds
// under load (the ps exiting and a goroutine waking), against a pid that would
// have to be reused in that time. A child that could not be checked is left
// alone and named in the result: unverified means not killed, never unreported.
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
	var unchecked []int
	for i, r := range rows {
		if r.zombie {
			continue // dead already; only its parent's Wait is missing
		}
		if ctx.Err() != nil {
			// The shared bound ran out before this child's turn.
			for _, rest := range rows[i:] {
				if !rest.zombie {
					unchecked = append(unchecked, rest.pid)
				}
			}
			break
		}
		// Re-read now: the listing is stale, and a pid is only ours while its
		// parent is.
		ppid, exists, err := parentOf(ctx, ps, r.pid)
		switch {
		case err != nil:
			// Could not verify. Not killed, which is right, and not silent
			// either: a check that did not run is not one that passed.
			unchecked = append(unchecked, r.pid)
			continue
		case !exists || ppid != self:
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
		if !gone(ctx, ps, r) {
			left = append(left, r.pid)
		}
	}
	var problems []string
	if len(unchecked) > 0 {
		problems = append(problems, fmt.Sprintf(
			"%d child process(es) could not be checked and were left running: %v", len(unchecked), unchecked))
	}
	if len(left) > 0 {
		problems = append(problems, fmt.Sprintf("child processes %v were still running at exit and could not be ended", left))
	}
	return strings.Join(problems, "; ")
}

// gonePoll is how often a killed child is looked for, which costs a ps each time.
const gonePoll = 50 * time.Millisecond

// gone waits, until ctx ends, for the child to be gone. A child that has died
// but not been waited for is a zombie, which kill(2) still finds, so it counts
// as gone too: a group is gone when no member of it is anything but a zombie.
// Its parent's Wait is what removes it, in the goroutine that abandoned it.
func gone(ctx context.Context, ps string, r childRow) bool {
	target := r.pid
	if r.pgid == r.pid {
		target = -r.pid
	}
	for {
		if err := syscall.Kill(target, 0); err == syscall.ESRCH {
			return true
		}
		if allZombies(ctx, ps, r) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(gonePoll):
		}
	}
}

// allZombies reports whether every member of r's group (or r alone, when it
// does not lead one) is a zombie or gone, from one read of the table. A table
// that cannot be read answers false: not shown to be gone.
func allZombies(ctx context.Context, ps string, r childRow) bool {
	out, _, err := runPS(ctx, ps, "-A", "-o", "pid=,pgid=,stat=")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		pgid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		member := pid == r.pid
		if r.pgid == r.pid {
			member = pgid == r.pid
		}
		if member && !strings.HasPrefix(f[2], "Z") {
			return false
		}
	}
	return true
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
	cmd.Env = childenv.Inherit() // ps needs no provider credential
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

// parentOf reads one process's parent. ps exits 1 when the pid does not exist,
// which is an answer (exists is false) once kill(2) agrees; any other failure, or a timeout, is an
// error and the process is unverified.
func parentOf(ctx context.Context, ps string, pid int) (ppid int, exists bool, err error) {
	out, _, err := runPS(ctx, ps, "-o", "ppid=", "-p", strconv.Itoa(pid))
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		// Believed only when the pid really is gone: a ps that exits 1 for a
		// process that still answers kill(2) has not said anything about it.
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return 0, false, nil
		}
		return 0, false, err
	}
	if err != nil {
		return 0, false, err
	}
	ppid, err = strconv.Atoi(strings.TrimSpace(out))
	return ppid, err == nil, err
}
