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
// process that exits while the scan runs (ENOENT or ESRCH on its stat) is
// skipped; it was not a member when its stat was unreadable. Any other read
// error (EACCES under hidepid=1, say) returns an error, since an unreadable
// process may be a member (listStatError).
//
// The trade-off on a host mounted with hidepid=1: other users' /proc/<pid>/stat
// is EACCES or EPERM, so every listing there fails, membersKnown stays false
// and groupLive answers from the bare kill(-pgid, 0) probe alone. That is
// fail-closed and costs at most one reapBudget, but it loses the distinction
// the listing exists for: a stranger that took the group number after the
// members died reads as the agent's live group.
//
// Under hidepid=2 other users' pids are absent from the directory listing
// altogether, so no read fails and the listing SUCCEEDS without them. A member
// that is hidden this way (one that changed uid after the agent started) is
// simply not in the snapshot, and membersKnown is true. groupLive then answers
// from the snapshotted members only (anyMemberInGroup), so the bare probe is
// NOT a backstop for it: once the visible members are gone the group reads
// free while the hidden one lives, which is the fail-open the card describes.
// This is a known gap and a design question, left open here on purpose.
//
// Every hidepid statement above is inferred from the code and was not run on
// Linux.
//
// What that error does: sweepGroup records membersKnown = false when either of
// its two listings fails, groupLive then answers from the bare probe alone (the
// group reads live while kill(2) finds a member, which is the fail-closed
// answer), and awaitGroup, the only waiter, polls that until its own
// reapBudget deadline and returns "still has a live member". So an error costs
// at most one reapBudget in close() and cannot hang the exit. Zombies are
// listed, as kill(2) counts them.
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
			if skip := listStatError(pid, err); skip != nil {
				return nil, skip
			}
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
