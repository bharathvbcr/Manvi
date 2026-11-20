//go:build linux

package codingagent

import (
	"fmt"
	"os"
	"strings"
)

// processZombie reads the state letter, the first field after the command's
// closing parenthesis in /proc/<pid>/stat: Z is exited and not reaped, X is dead.
func processZombie(pid int) (bool, error) {
	raw, err := readProcStat(pid)
	if err != nil {
		if !isAbsent(err) {
			return false, fmt.Errorf("read process %d: %w", pid, err)
		}
		if _, selfErr := readProcStat(os.Getpid()); selfErr != nil {
			return false, fmt.Errorf("process %d is not listed and /proc is not readable: %w", pid, selfErr)
		}
		return false, ErrNoProcess
	}
	end := strings.LastIndexByte(raw, ')')
	if end < 0 || end+2 >= len(raw) {
		return false, fmt.Errorf("process %d: /proc/%d/stat did not parse", pid, pid)
	}
	state := raw[end+2]
	return state == 'Z' || state == 'X', nil
}

// processStart reads field 22 of /proc/<pid>/stat. ENOENT and ESRCH mean the
// process is gone, but only while /proc itself is readable, as memberInGroup
// requires: this process's own entry must read, or an unmounted /proc would make
// every process look gone.
func processStart(pid int) (int64, error) {
	raw, err := readProcStat(pid)
	if err != nil {
		if !isAbsent(err) {
			return 0, fmt.Errorf("read process %d: %w", pid, err)
		}
		if _, selfErr := readProcStat(os.Getpid()); selfErr != nil {
			return 0, fmt.Errorf("process %d is not listed and /proc is not readable: %w", pid, selfErr)
		}
		return 0, ErrNoProcess
	}
	_, start, ok := parseProcStat(raw)
	if !ok {
		return 0, fmt.Errorf("process %d: /proc/%d/stat did not parse", pid, pid)
	}
	return start, nil
}
