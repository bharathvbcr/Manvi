//go:build darwin

package budget

import "golang.org/x/sys/unix"

// processStartUnix is when pid's process started, in Unix seconds, from the
// kernel's process table. A pid that names nothing, or a failed lookup, is not
// a start time.
func processStartUnix(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	p, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(p.Proc.P_pid) != pid {
		return 0, false
	}
	return p.Proc.P_starttime.Sec, true
}
