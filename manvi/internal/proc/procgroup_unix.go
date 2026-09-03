//go:build unix

package proc

import (
	"os/exec"
	"syscall"
)

// ConfigureGroup puts a child in its own process group so a timeout can reach
// anything it spawned.
//
// Killing only the direct child leaves a grandchild running and holding the
// stdout pipe, which is the difference between a bounded call and a harness
// that stops responding. WaitDelay alone unblocks this harness; the group kill
// also stops the orphan from continuing to run against whatever it was given.
//
// This lives beside RunBounded because it is the same lesson at the same seam,
// and because it had been learned at only two of the five places that exec a
// child: the store and the shell tool had it, while the verifier, the repo map
// and the searcher did not. A hardening applied at some call sites is a
// hardening the next call site will be written without.
func ConfigureGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid addresses the group. If the group is gone, fall back to
		// the process itself rather than reporting a failure to cancel.
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}

// ConfigureOwnGroup runs a child as its own process-group leader without arming
// a cancel.
//
// This is ConfigureGroup's counterpart for a child that outlives a single call.
// ConfigureGroup sets cmd.Cancel, which os/exec accepts only on a command built
// with CommandContext and which is the right shape for a one-shot invocation
// bounded by its caller's deadline. A persistent child has no such deadline to
// be killed by — it is ended explicitly — so the group is set here and
// addressed by KillGroup at that point.
//
// Two things follow from the group, and both are why this is not merely
// Setpgid inline at the call site. The owner can reach descendants the child
// spawned, which it otherwise could not: killing the direct child leaves a
// daemon it started running with the inherited pipes. And a group kill aimed at
// the child cannot reach this harness — sharing manvi's group made "kill the
// child's group" and "kill manvi" the same act.
func ConfigureOwnGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillGroup SIGKILLs the group led by pid. Best-effort by design: the group may
// already be gone, and reporting a failure to kill an already-dead process
// helps nobody.
func KillGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
