//go:build unix

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// setCloseOnExec marks fd close-on-exec, so no process this one starts holds
// it. It fails on a descriptor that is not open, which is how a mistyped
// --approve-fd is told apart from a real one before anything is spawned.
func setCloseOnExec(fd int) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("not an open file descriptor: %w", err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		return nil
	}
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("marking it close-on-exec: %w", err)
	}
	return nil
}
