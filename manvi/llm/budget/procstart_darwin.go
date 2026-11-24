//go:build darwin

package budget

import "golang.org/x/sys/unix"

// szomb is p_stat for a process that has exited and not been reaped (SZOMB in
// <sys/proc.h>).
const szomb = 5

// processZombie reports whether pid is a zombie: its pid still answers
// kill(pid, 0), but nothing can settle what it reserved. Only a definite answer
// counts.
func processZombie(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && p != nil && int(p.Proc.P_pid) == pid && p.Proc.P_stat == szomb
}

// processStartUnix is when pid's process started, in Unix seconds, from the
// kernel's process table. A pid that names nothing, or a failed lookup, is not
// a start time.
func processStartUnix(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || p == nil || int(p.Proc.P_pid) != pid {
		return 0, false
	}
	return p.Proc.P_starttime.Sec, true
}
