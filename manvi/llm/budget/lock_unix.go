//go:build unix

package budget

import (
	"errors"
	"syscall"
)

// processGone reports whether pid names no running process. Only a definite
// "no such process" counts: EPERM means it exists under another user, and any
// other error is not proof, so the attempt it owns is left alone.
func processGone(pid int) bool {
	if pid <= 0 {
		// Never signal pid 0 or a negative pid: those address process groups.
		// No live process can own such an attempt.
		return true
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
