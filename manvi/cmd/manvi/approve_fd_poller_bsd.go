//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import "golang.org/x/sys/unix"

// isRuntimePoller reports whether fd is a kqueue. On these systems fstat gives
// a kqueue the type of a FIFO, so the type check cannot tell it from the
// host's pipe; kevent, which only a kqueue accepts, can. Package initialisers
// run before main and can open a file, which starts the Go runtime's poller on
// the lowest free descriptor number, and a closed --approve-fd 3 is then
// reissued as that kqueue.
func isRuntimePoller(fd int) bool {
	_, err := unix.Kevent(fd, nil, nil, &unix.Timespec{})
	return err == nil
}
