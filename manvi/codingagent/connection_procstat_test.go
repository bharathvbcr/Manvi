//go:build unix

package codingagent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// memberInGroup decides from one stat read whether a snapshotted member is
// still in the group. Gone means the process is gone or was replaced; anything
// that could not be read or understood counts as present, so a poll that cannot
// tell reads the group live.
func TestMemberInGroupClassifiesEveryStatOutcome(t *testing.T) {
	const pgid, start = 777, 987654
	stat := func(group int, st int64) string {
		return fmt.Sprintf("4242 (x) S 1 %d 4242 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 %d 1000 10", group, st)
	}
	m := groupMember{pid: 4242, start: start}
	cases := []struct {
		name string
		stat string
		err  error
		// selfErr makes the read of this process's own entry fail: absent is what
		// an unmounted /proc looks like; any other failure proves nothing either.
		selfErr error
		want    bool
	}{
		{"still the same member", stat(pgid, start), nil, nil, true},
		{"ENOENT means gone", "", &fs.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.ENOENT}, nil, false},
		{"fs.ErrNotExist means gone", "", fs.ErrNotExist, nil, false},
		{"ESRCH means gone", "", &fs.PathError{Op: "read", Path: "/proc/4242/stat", Err: syscall.ESRCH}, nil, false},
		{"EACCES fails closed", "", &fs.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.EACCES}, nil, true},
		{"EPERM fails closed", "", os.ErrPermission, nil, true},
		{"an unrecognised error fails closed", "", errors.New("boom"), nil, true},
		{"a stat that does not parse fails closed", "garbage", nil, nil, true},
		{"an empty stat fails closed", "", nil, nil, true},
		{"member ENOENT but /proc missing counts live", "", &fs.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.ENOENT}, &fs.PathError{Op: "open", Path: "/proc/self/stat", Err: syscall.ENOENT}, true},
		{"member ESRCH but /proc missing counts live", "", &fs.PathError{Op: "read", Path: "/proc/4242/stat", Err: syscall.ESRCH}, &fs.PathError{Op: "open", Path: "/proc/self/stat", Err: syscall.ENOENT}, true},
		{"member ENOENT but this process's own /proc read fails otherwise (EACCES) counts live", "", &fs.PathError{Op: "open", Path: "/proc/4242/stat", Err: syscall.ENOENT}, &fs.PathError{Op: "open", Path: "/proc/self/stat", Err: syscall.EACCES}, true},
		{"moved to another group", stat(pgid+1, start), nil, nil, false},
		{"same pid, different start (pid reuse)", stat(pgid, start+1), nil, nil, false},
	}
	for _, tc := range cases {
		read := func(pid int) (string, error) {
			if pid == os.Getpid() {
				if tc.selfErr != nil {
					return "", tc.selfErr
				}
				return stat(os.Getpid(), 1), nil
			}
			if pid != m.pid {
				t.Errorf("%s: read pid %d, want %d", tc.name, pid, m.pid)
			}
			return tc.stat, tc.err
		}
		if got := memberInGroup(read, pgid, m); got != tc.want {
			t.Errorf("%s: memberInGroup = %v, want %v", tc.name, got, tc.want)
		}
	}
}
