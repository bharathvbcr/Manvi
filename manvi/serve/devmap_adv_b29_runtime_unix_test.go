//go:build unix

package serve

import "syscall"

func syscallAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func syscallKill(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
