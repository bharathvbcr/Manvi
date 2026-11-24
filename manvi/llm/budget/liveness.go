package budget

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"sync"
)

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

// startSlackSeconds is how far a process's start may trail the one recorded
// before the pid is taken to belong to another process. Both are whole seconds
// read from the same kernel field, so the slack only has to cover rounding. A
// larger disagreement misjudges a live owner as dead, which retains its spend
// and never loses it.
//
// On Linux the start time is the process's ticks since boot added to btime, and
// the kernel derives btime from the wall clock at the moment it is read, so a
// clock step moves every process's start time by the size of the step. Two
// reads of one process taken across a step therefore disagree by that step, and
// a step larger than the slack can make a live foreign owner look replaced.
// Comparing against a start time stored at reservation (and not against
// time.Now()) is what keeps an ordinary skew between the wall clock and the
// reservation from mattering; the step case remains and fails safe.
const startSlackSeconds = 5

// maxOwnerTokenLen bounds a stored owner token.
const maxOwnerTokenLen = 64

// ownerToken identifies this process in the records it writes. It is random, so
// a later process that is given this one's pid has a different one.
var ownerToken = newOwnerToken()

func newOwnerToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not return an error since Go 1.24
	return hex.EncodeToString(b[:])
}

// ownStartUnix is this process's own start time, read once: it is stored in
// every record this process writes. Zero when the platform cannot say.
var ownStartUnix = sync.OnceValue(func() int64 {
	start, _ := processStartUnix(os.Getpid())
	return start
})

// ownerGone reports whether the process that reserved a can no longer settle
// it: its pid names nothing or a zombie, or names a process that started after
// the record's owner did, which means the pid was reused by a stranger.
//
// A record this process wrote is never gone, however the wall clock has moved
// since: the start-time rule is not applied to it. Ownership is the token, not
// the pid; a record with this pid and another token was written by an earlier
// process that had the same pid, and is gone. A record without a token keeps the
// pid rule: it is this process's if the pid is.
func ownerGone(a Attempt) bool {
	self := os.Getpid()
	switch {
	case a.Owner != "" && a.Owner == ownerToken:
		return false
	case a.Owner == "" && a.PID == self:
		return false
	case a.PID == self:
		return true
	}
	return processGone(a.PID) || processZombie(a.PID) || startedAfter(a, processStartUnix)
}

// startedAfter reports whether the process now holding a's pid started later
// than a's owner did. The owner's own start time is the reference when the
// record has one, else the time the record was made. Only a definite start time
// counts; an unreadable one, or a record with no time, says nothing.
func startedAfter(a Attempt, startOf func(pid int) (unix int64, ok bool)) bool {
	reference := a.OwnerStart
	if reference <= 0 {
		reference = a.StartedUnix
	}
	if reference <= 0 {
		return false
	}
	start, ok := startOf(a.PID)
	return ok && start > reference+startSlackSeconds
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
