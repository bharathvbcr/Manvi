//go:build unix

package main

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func closeOnExecOf(t *testing.T, f *os.File) bool {
	t.Helper()
	flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	return flags&unix.FD_CLOEXEC != 0
}

// The claim is what keeps the descriptor out of every child, and it is a no-op
// when the flag was not given.
func TestClaimApproveFDMarksTheDescriptorCloseOnExec(t *testing.T) {
	read, _ := rawPipe(t)
	// rawPipe uses syscall.Pipe, whose descriptors are inheritable, as a
	// host's are.
	if _, err := unix.FcntlInt(read.Fd(), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	if closeOnExecOf(t, read) {
		t.Fatal("precondition: the descriptor is already close-on-exec")
	}

	if err := claimApproveFD(runOptions{approveFD: int(read.Fd())}); err != nil {
		t.Fatal(err)
	}
	if closeOnExecOf(t, read) {
		t.Fatal("claimApproveFD acted without --approve-fd")
	}

	if err := claimApproveFD(runOptions{approveFD: int(read.Fd()), approveFDSet: true}); err != nil {
		t.Fatal(err)
	}
	if !closeOnExecOf(t, read) {
		t.Fatal("the approval descriptor is still inheritable after claimApproveFD")
	}
	// Idempotent: a descriptor that is already marked is left alone.
	if err := claimApproveFD(runOptions{approveFD: int(read.Fd()), approveFDSet: true}); err != nil {
		t.Fatal(err)
	}
}

func TestClaimApproveFDRefusesADescriptorThatIsNotOpen(t *testing.T) {
	read, write := rawPipe(t)
	fd := int(read.Fd())
	_ = write.Close()
	if err := read.Close(); err != nil {
		t.Fatal(err)
	}
	err := claimApproveFD(runOptions{approveFD: fd, approveFDSet: true})
	if err == nil {
		t.Fatalf("a closed descriptor %d was accepted", fd)
	}
	if got := err.Error(); len(got) < len("--approve-fd") || got[:len("--approve-fd")] != "--approve-fd" {
		t.Fatalf("the error does not name the flag: %v", err)
	}
}
