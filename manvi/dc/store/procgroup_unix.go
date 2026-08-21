//go:build unix

package store

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the store in its own process group so a timeout
// can reach anything it spawned.
//
// Killing only the direct child leaves a grandchild running and holding the
// stdout pipe, which is the difference between a bounded call and a harness
// that stops responding. WaitDelay alone unblocks this harness; the group kill
// also stops the orphan from continuing to run against the database.
func configureProcessGroup(cmd *exec.Cmd) {
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
