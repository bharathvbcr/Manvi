//go:build unix

package codingagent

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// memberInGroup says whether m, as snapshotted, is still in group pgid, from its
// /proc/<pid>/stat as read by read. ENOENT and ESRCH mean the process is gone,
// but only while /proc itself is readable: the same read of this process's own
// entry must succeed or fail with something other than "absent", otherwise an
// unmounted or unreachable /proc would make every member look gone. A different
// group or start time means the pid is gone or was reused. Any other failure
// (EACCES under hidepid=1, a stat that does not parse) proves nothing either
// way, so the member counts as present: the group reads live, as the bare probe
// did. Under hidepid=2 another user's member also reads ENOENT, which is what the
// bare listing reported on main.
func memberInGroup(read func(pid int) (string, error), pgid int, m groupMember) bool {
	raw, err := read(m.pid)
	if err != nil {
		if !isAbsent(err) {
			return true
		}
		_, selfErr := read(os.Getpid())
		return isAbsent(selfErr)
	}
	group, start, ok := parseProcStat(raw)
	if !ok {
		return true
	}
	return group == pgid && start == m.start
}

// isAbsent reports whether err says the /proc entry does not exist.
func isAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// parseProcStat returns the process group (field 5) and start time (field 22)
// of one /proc/<pid>/stat line.
func parseProcStat(line string) (pgrp int, start int64, ok bool) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return 0, 0, false
	}
	// After ")": state is field 3, so field n is index n-3.
	fields := strings.Fields(line[end+1:])
	if len(fields) < 20 {
		return 0, 0, false
	}
	pgrp, err := strconv.Atoi(fields[2])
	if err != nil {
		return 0, 0, false
	}
	start, err = strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return pgrp, start, true
}
