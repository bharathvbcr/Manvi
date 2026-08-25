//go:build unix

package mcp

import (
	"os/exec"
	"syscall"
)

// setOwnProcessGroup runs the server as its own process-group leader.
//
// Two things follow. Close can address the descendants the server spawned,
// which it otherwise could not: killing the direct child leaves a daemon it
// started running with the inherited pipes. And a group kill aimed at the
// server cannot reach this harness — sharing manvi's group made "kill the
// server's group" and "kill manvi" the same act.
func setOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the group led by pid. Best-effort by design: the
// group may already be gone, and reporting a failure to kill an already-dead
// process helps nobody.
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
