//go:build unix

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// A descriptor this process opened is close-on-exec (Go opens every file that
// way), as is the cgroup file the Linux runtime opens at startup and which takes
// fd 3 when the host left fd 3 closed. The pre-scan is the first claim of a real
// start, so a close-on-exec descriptor there was not inherited and is refused.
func TestClaimApproveFDFromArgvRefusesADescriptorThisProcessOpened(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "not-inherited"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if !closeOnExecOf(t, f) {
		t.Fatal("precondition: a file this process opened is close-on-exec")
	}
	fd := strconv.Itoa(int(f.Fd()))
	err = claimApproveFDFromArgv([]string{"run", "-p", "x", "--json", "--approve-fd", fd})
	if err == nil {
		t.Fatalf("--approve-fd %s, a descriptor this process opened, was accepted", fd)
	}
	if !strings.HasPrefix(err.Error(), "--approve-fd "+fd) || !strings.Contains(err.Error(), "close-on-exec") {
		t.Fatalf("the refusal does not name the flag and the reason: %v", err)
	}
}

// The control: an inheritable descriptor, which is what a host passes, is
// accepted by the pre-scan and leaves it close-on-exec.
func TestClaimApproveFDFromArgvAcceptsAnInheritableDescriptor(t *testing.T) {
	read, _ := rawPipe(t)
	if _, err := unix.FcntlInt(read.Fd(), unix.F_SETFD, 0); err != nil {
		t.Fatal(err)
	}
	fd := strconv.Itoa(int(read.Fd()))
	if err := claimApproveFDFromArgv([]string{"run", "-p", "x", "--json", "--approve-fd", fd}); err != nil {
		t.Fatalf("an inheritable --approve-fd %s was refused: %v", fd, err)
	}
	if !closeOnExecOf(t, read) {
		t.Fatal("the pre-scan accepted the descriptor but left it inheritable")
	}
}
