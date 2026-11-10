//go:build unix

package codingagent

import (
	"errors"
	"io/fs"
	"strconv"
	"strings"
	"syscall"
)

// memberInGroup says whether m, as snapshotted, is still in group pgid, from its
// /proc/<pid>/stat as read by read. ENOENT and ESRCH mean the process is gone.
// A different group or start time means the pid is gone or was reused. Any other
// failure (EACCES under hidepid, a stat that does not parse) proves nothing
// either way, so the member counts as present: the group reads live, as the bare
// probe did.
func memberInGroup(read func(pid int) (string, error), pgid int, m groupMember) bool {
	raw, err := read(m.pid)
	if err != nil {
		return !(errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH))
	}
	group, start, ok := parseProcStat(raw)
	if !ok {
		return true
	}
	return group == pgid && start == m.start
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
