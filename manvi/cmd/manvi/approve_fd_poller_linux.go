//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// isRuntimePoller reports whether fd is one of the Go runtime's poller
// descriptors: the epoll instance or the eventfd that wakes it. Linux gives both
// the type of a regular file, so the type check cannot tell them from the host's
// file. epoll_wait, which only an epoll descriptor accepts, names the first. The
// second is not an epoll descriptor, and which of the two takes the lowest free
// number depends on the runtime, so /proc names it: every anonymous-inode
// descriptor (eventpoll, eventfd) is link text "anon_inode:[...]", and none is a
// pipe, socket, character device or file a host can hand over. A closed
// --approve-fd 3 is reissued as one of them once a package initialiser has
// opened a file.
func isRuntimePoller(fd int) bool {
	if _, err := unix.EpollWait(fd, make([]unix.EpollEvent, 1), 0); err == nil {
		return true
	}
	target, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	return err == nil && strings.HasPrefix(target, "anon_inode:")
}
