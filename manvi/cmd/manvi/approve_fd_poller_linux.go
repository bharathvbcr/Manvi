//go:build linux

package main

import "golang.org/x/sys/unix"

// isRuntimePoller reports whether fd is an epoll instance, which is what the
// Go runtime's poller is. Linux gives it the type of a regular file, so the
// type check cannot tell it from the host's file; epoll_wait, which only an
// epoll descriptor accepts, can. A closed --approve-fd 3 is reissued as this
// descriptor once a package initialiser has opened a file.
func isRuntimePoller(fd int) bool {
	_, err := unix.EpollWait(fd, make([]unix.EpollEvent, 1), 0)
	return err == nil
}
