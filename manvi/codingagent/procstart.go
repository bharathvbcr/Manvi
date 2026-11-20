package codingagent

import "errors"

// ErrNoProcess is ProcessStart's answer for a pid that names no process. It is
// the only answer that proves a process is gone: every other error leaves its
// state unknown.
var ErrNoProcess = errors.New("no such process")

// ErrProcessStartUnsupported is ProcessStart's answer on a platform that has no
// way of reading when another process started. A caller must treat it as "cannot
// tell", never as "gone".
var ErrProcessStartUnsupported = errors.New("reading a process's start time is not supported on this platform")

// ProcessStart is when pid started, in a unit that is only comparable with
// another ProcessStart of the same pid on the same boot: clock ticks since boot
// on Linux, microseconds since the epoch on macOS. A pid is handed out again
// once its process is gone, so a pid alone cannot say whether the process that
// was recorded is the one running now; a pid with the start time it was
// recorded with can. It returns ErrNoProcess when no process has the pid and
// ErrProcessStartUnsupported where the platform cannot answer.
func ProcessStart(pid int) (int64, error) {
	if pid <= 0 {
		// Never a process: pid 0 and negative pids address groups elsewhere.
		return 0, ErrNoProcess
	}
	return processStart(pid)
}
