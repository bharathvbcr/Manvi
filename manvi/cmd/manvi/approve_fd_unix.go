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
	switch uint32(st.Mode) & unix.S_IFMT {
	case unix.S_IFIFO, unix.S_IFSOCK, unix.S_IFCHR, unix.S_IFREG:
	default:
		return fmt.Errorf("not a pipe, socket, character device or file (mode %#o), so it cannot be the host's channel", uint32(st.Mode))
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
