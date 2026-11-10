//go:build linux

package codingagent

import (
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
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

// readProcStat reads one /proc/<pid>/stat with three syscalls and no
// allocation beyond the string: os.ReadFile would stat the file and try to
// register it with the runtime's poller first, which this poll, run every
// groupPoll for every snapshotted member, does not need. The errno is returned
// as is, for memberInGroup to classify.
func readProcStat(pid int) (string, error) {
	fd, err := unix.Open("/proc/"+strconv.Itoa(pid)+"/stat", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", err
	}
	defer unix.Close(fd) //nolint:errcheck // a read-only descriptor
	var buf [1024]byte
	n, err := unix.Read(fd, buf[:])
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
