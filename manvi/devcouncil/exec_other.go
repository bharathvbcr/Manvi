//go:build !unix

package devcouncil

import "os/exec"

// On platforms without process groups the deadline still bounds the direct
// child, and cmd.WaitDelay (set by runShellCommand) bounds how long leftover
// descendants may hold the stdio pipes before Wait gives up on them.
func setOwnProcessGroup(cmd *exec.Cmd) {}

func killProcessGroup(pid int) {}
