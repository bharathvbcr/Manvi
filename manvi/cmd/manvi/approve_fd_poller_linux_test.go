//go:build linux

package main

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The runtime's poller is an epoll instance and an eventfd, and either can be
// the lowest free number a closed --approve-fd is reissued as. A host's channel
// (a pipe) is neither.
func TestIsRuntimePollerNamesEpollAndEventfdButNotAPipe(t *testing.T) {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(epfd) })
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(efd) })
	var pipe [2]int
	if err := unix.Pipe2(pipe[:], unix.O_CLOEXEC); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(pipe[0]); _ = unix.Close(pipe[1]) })

	for name, fd := range map[string]int{"epoll": epfd, "eventfd": efd} {
		if !isRuntimePoller(fd) {
			t.Errorf("%s descriptor %d was not recognised as the runtime's poller", name, fd)
		}
		if err := prepareApproveFD(fd); err == nil {
			t.Errorf("%s descriptor %d was accepted as an approval channel", name, fd)
		}
	}
	for name, fd := range map[string]int{"pipe read end": pipe[0], "pipe write end": pipe[1]} {
		if isRuntimePoller(fd) {
			t.Errorf("%s was taken for the runtime's poller", name)
		}
		if err := prepareApproveFD(fd); err != nil {
			t.Errorf("%s refused as an approval channel: %v", name, err)
		}
	}
}
