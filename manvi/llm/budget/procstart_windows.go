//go:build windows

package budget

import "golang.org/x/sys/windows"

// processZombie: Windows has no zombie state; an exited process is reported by
// processGone.
func processZombie(int) bool { return false }

// processStartUnix is when pid's process was created, in Unix seconds, from
// GetProcessTimes. A process that cannot be opened, or a failed query, is not
// a start time.
func processStartUnix(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exited, &kernel, &user); err != nil {
		return 0, false
	}
	return created.Nanoseconds() / 1e9, true
}
