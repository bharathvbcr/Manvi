//go:build linux

package codingagent

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// groupMembers lists the processes whose process group is pgid with each one's
// start time, by scanning /proc/<pid>/stat: field 5 is the process group and
// field 22 the start time in clock ticks since boot. comm (field 2) may hold
// spaces and parentheses, so fields are counted from after the last ')'. A
// process that exits while the scan runs is skipped; it was not a member when
// its stat was unreadable. Zombies are listed, as kill(2) counts them.
func groupMembers(pgid int) ([]groupMember, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("list process group %d: %w", pgid, err)
	}
	var members []groupMember
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		group, start, ok := parseProcStat(string(raw))
		if ok && group == pgid {
			members = append(members, groupMember{pid: pid, start: start})
		}
	}
	return members, nil
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
