package budget

import "os"

// The decision half of the Windows process-liveness probe. The syscalls that
// feed it live in lock_windows.go; the rule lives here, in a file every
// platform builds, so a unit test can run it anywhere. lock_windows.go checks
// these constants against golang.org/x/sys/windows at compile time.
const (
	// stillActive is what GetExitCodeProcess reports for a running process
	// (STILL_ACTIVE, which is STATUS_PENDING). It is also an ordinary exit code
	// a process may choose, so on its own it cannot tell the two apart.
	stillActive = 259
	// waitObject0 is WaitForSingleObject's answer for a signaled object, and a
	// process object is signaled once the process has exited.
	waitObject0 = 0
	// waitTimeout is its answer for an object still unsignaled when the wait
	// ran out: a running process, for a zero-length wait.
	waitTimeout = 258
)

// startSlackSeconds is how far a process's start may trail the time recorded
// on its attempt before the pid is taken to belong to another process. A real
// owner started before it reserved anything, so the slack only has to cover the
// whole-second granularity of both clocks and a small clock step; a larger step
// misjudges a live owner as dead, which retains its spend and never loses it.
const startSlackSeconds = 5

// ownerGone reports whether the process that reserved a can no longer settle
// it: its pid names nothing, or names a process that started after the attempt
// was recorded, which means the pid was reused by a stranger. The current
// process's own pid is never "gone", but a record that predates this process
// (left by an earlier one that held the same pid) is replaced like any other.
func ownerGone(a Attempt) bool {
	if a.PID != os.Getpid() && processGone(a.PID) {
		return true
	}
	return startedAfter(a, processStartUnix)
}

// startedAfter reports whether the process now holding a's pid started later
// than a was recorded. Only a definite start time counts; an unreadable one, or
// a record with no time, says nothing.
func startedAfter(a Attempt, startOf func(pid int) (unix int64, ok bool)) bool {
	if a.StartedUnix <= 0 {
		return false
	}
	start, ok := startOf(a.PID)
	return ok && start > a.StartedUnix+startSlackSeconds
}

// exitProbe is everything the Windows probe learned about one pid.
type exitProbe struct {
	// OpenErr is OpenProcess's failure, nil when it opened. NoSuchProcess says
	// the failure was ERROR_INVALID_PARAMETER, which is how Windows answers for
	// a pid that names nothing. Any other failure (access denied, most often)
	// proves nothing about the process.
	OpenErr       error
	NoSuchProcess bool
	// ExitCode and ExitCodeErr are GetExitCodeProcess's answer.
	ExitCode    uint32
	ExitCodeErr error
	// Wait is a zero-length WaitForSingleObject on the same handle. It is asked
	// only when the exit code is stillActive, the one value that is ambiguous.
	Wait func() (event uint32, err error)
}

// gone reports whether the pid names no running process. Only a definite
// answer counts, so every doubt is "alive" and the attempt the process owns is
// left alone: an unopenable process, an unreadable exit code and a failed wait
// all keep it.
func (p exitProbe) gone() bool {
	if p.OpenErr != nil {
		return p.NoSuchProcess
	}
	if p.ExitCodeErr != nil {
		return false
	}
	if p.ExitCode != stillActive {
		return true
	}
	// 259 is either a running process or one that exited with code 259. Without
	// this check the second kind was never reclaimed, and its reservation held
	// the cap for good. Only a signaled process object means it exited.
	if p.Wait == nil {
		return false
	}
	event, err := p.Wait()
	return err == nil && event == waitObject0
}
