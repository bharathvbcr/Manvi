//go:build darwin

package codingagent

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// groupMembers lists the processes whose process group is pgid with each one's
// start time, from sysctl kern.proc.pgrp. Zombies are listed too, as they are
// by kill(2). The start time is what tells a member from a later process that
// was handed the same pid.
func groupMembers(pgid int) ([]groupMember, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return nil, fmt.Errorf("list process group %d: %w", pgid, err)
	}
	members := make([]groupMember, 0, len(procs))
	for _, p := range procs {
		if int(p.Eproc.Pgid) != pgid {
			continue
		}
		members = append(members, groupMember{pid: int(p.Proc.P_pid), start: int64(p.Proc.P_starttime.Sec)*1_000_000 + int64(p.Proc.P_starttime.Usec)})
	}
	return members, nil
}
