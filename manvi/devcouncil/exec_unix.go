//go:build unix

package devcouncil

import (
	"os/exec"
	"syscall"
)

// setOwnProcessGroup runs the command as its own process-group leader, so its
// descendants are addressable as a group when the deadline fires.
//
// CommandContext kills only the direct child. An agent command that
// backgrounds work (`sleep infinity &`, a daemon it started) leaves that
// descendant holding the inherited stdout pipe: os/exec's copy goroutine then
// blocks on EOF forever and Wait never returns, wedging the whole tool call
// past its own timeout. Killing the group reaches descendants that killing the
// child alone cannot.
func setOwnProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup SIGKILLs the process group led by pid. Best-effort by
// design: the group may already be gone, and reporting a failure to kill an
// already-dead process helps nobody.
func killProcessGroup(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
