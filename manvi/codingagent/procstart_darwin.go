//go:build darwin

package codingagent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// processZombie reads p_stat from the same record: SZOMB is exited and not
// reaped.
func processZombie(pid int) (bool, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return false, fmt.Errorf("read process %d: %w", pid, err)
	}
	for _, p := range procs {
		if int(p.Proc.P_pid) == pid {
			return p.Proc.P_stat == szomb, nil
		}
	}
	return false, ErrNoProcess
}

// szomb is SZOMB from <sys/proc.h>: the process has exited and its parent has
// not yet waited for it.
const szomb = 5

// processStart reads the process's start time from sysctl kern.proc.pid, in the
// same unit groupMembers records. The slice form is used because the single
// form answers a pid with no process as EIO, which is not proof of anything;
// an empty slice is the kernel saying there is none.
func processStart(pid int) (int64, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("read process %d: %w", pid, err)
	}
	for _, p := range procs {
		if int(p.Proc.P_pid) == pid {
			return p.Proc.P_starttime.Sec*1_000_000 + int64(p.Proc.P_starttime.Usec), nil
		}
	}
	return 0, ErrNoProcess
}
