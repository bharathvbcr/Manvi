//go:build unix

package main

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// prepareApproveFD checks that fd is something a host can hand over as a
// channel (a pipe or FIFO, a socket, a character device or a file) and marks
// it close-on-exec, so no process this one starts holds it.
//
// The type check is what tells a real descriptor from one the runtime took for
// itself: a --approve-fd that was closed can be reissued as the runtime's own
// kqueue (a kqueue has none of these types), and reading it would be reading
// the poller. A descriptor that is not open at all fails here too.
func prepareApproveFD(fd int) error {
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return fmt.Errorf("not an open file descriptor: %w", err)
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFCHR, unix.S_IFREG:
	default:
		return fmt.Errorf("not a pipe, socket, character device or file (mode %#o), so it cannot be the host's channel", st.Mode)
	}
	if isRuntimePoller(fd) {
		return errors.New("that number is the runtime's own event poller, not a descriptor the host passed: the descriptor was not open when this process started")
	}
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

// inheritedAtExec refuses a descriptor that is already close-on-exec when the
// argv pre-scan first looks at it. exec closes every close-on-exec descriptor,
// so one that survived it never has the flag; a descriptor that does was opened
// by this process before the claim, by the runtime or a package initialiser,
// and took the number of an --approve-fd the host did not leave open. The type
// check and isRuntimePoller each name one such descriptor (the kqueue, the
// epoll instance); this covers every one, including the cgroup file the Linux
// runtime opens at startup, which is a regular file and passes both.
func inheritedAtExec(fd int) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("not an open file descriptor: %w", err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		return errors.New("it is close-on-exec, so it was not inherited: this process opened it before reading --approve-fd, which means the descriptor the host meant was not open when this process started")
	}
	return nil
}
