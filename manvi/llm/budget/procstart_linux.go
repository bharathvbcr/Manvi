//go:build linux

package budget

import (
	"bytes"
	"os"
	"strconv"
)

// userHZ is the unit of /proc/<pid>/stat start times. It is fixed at 100 for
// every Linux architecture userland sees, whatever the kernel's tick rate.
const userHZ = 100

// processStartUnix is when pid's process started, in Unix seconds: its start
// time in ticks since boot (field 22 of /proc/<pid>/stat) added to the boot
// time (btime in /proc/stat). Either being unreadable is not a start time.
func processStartUnix(pid int) (int64, bool) {
	if pid <= 0 {
		return 0, false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	ticks, ok := parseStartTicks(stat)
	if !ok {
		return 0, false
	}
	system, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, false
	}
	boot, ok := parseBootTime(system)
	if !ok {
		return 0, false
	}
	return boot + ticks/userHZ, true
}

// processZombie reports whether pid is a zombie (state Z in /proc/<pid>/stat):
// its pid still answers kill(pid, 0), but nothing can settle what it reserved.
// Only a definite answer counts.
func processZombie(pid int) bool {
	if pid <= 0 {
		return false
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return false
	}
	fields := bytes.Fields(stat[end+1:])
	return len(fields) > 0 && string(fields[0]) == "Z"
}

// parseStartTicks reads field 22 of a /proc/<pid>/stat line. comm (field 2) may
// hold spaces and parentheses, so fields are counted from after the last ')',
// where field 3 comes first.
func parseStartTicks(stat []byte) (int64, bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, false
	}
	fields := bytes.Fields(stat[end+1:])
	const field = 22 - 3
	if len(fields) <= field {
		return 0, false
	}
	ticks, err := strconv.ParseInt(string(fields[field]), 10, 64)
	return ticks, err == nil && ticks >= 0
}

// parseBootTime reads the btime line of /proc/stat.
func parseBootTime(stat []byte) (int64, bool) {
	for _, line := range bytes.Split(stat, []byte{'\n'}) {
		if rest, ok := bytes.CutPrefix(line, []byte("btime ")); ok {
			boot, err := strconv.ParseInt(string(bytes.TrimSpace(rest)), 10, 64)
			return boot, err == nil && boot > 0
		}
	}
	return 0, false
}
