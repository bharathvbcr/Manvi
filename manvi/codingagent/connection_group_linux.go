//go:build linux

package codingagent

import (
	"fmt"
	"os"
	"strconv"
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

// anyMemberInGroup says whether any of was is still in process group pgid,
// reading only those processes' /proc/<pid>/stat. Cost is the snapshot's size,
// not the machine's process count.
func anyMemberInGroup(pgid int, was []groupMember) bool {
	for _, m := range was {
		if memberInGroup(readProcStat, pgid, m) {
			return true
		}
	}
	return false
}

func readProcStat(pid int) (string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	return string(raw), err
}
