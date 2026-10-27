//go:build unix

package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// rawPipe is a pipe whose read end is a plain blocking descriptor, which is
// what a host's inherited fd 3 is. os.Pipe returns a non-blocking, pollable
// pair, and a read deadline works on that one; it does not on this.
func rawPipe(t *testing.T) (read, write *os.File) {
	t.Helper()
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	read = os.NewFile(uintptr(fds[0]), "approve-fd")
	write = os.NewFile(uintptr(fds[1]), "host-end")
	t.Cleanup(func() { _ = write.Close(); _ = read.Close() })
	return read, write
}

// Audit B: Approve stayed blocked after cancel because the read could not be
// interrupted on a blocking descriptor and the call waited for it.
func TestApproveFDCancelReturnsPromptlyOnABlockingDescriptor(t *testing.T) {
	read, _ := rawPipe(t)
	if err := read.SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		t.Skip("this descriptor supports read deadlines, so it is not the case under test")
	}
	log := &approvalEvents{}
	ap := newFDApprover(read, log)
	ctx, cancel := context.WithCancel(context.Background())
	done := pendingApproval(t, ap, log, ctx, ui.Request{ID: "a1", Reason: "x"})
	cancel()
	if d := receiveDecision(t, done, time.Second); d.Allow {
		t.Fatalf("cancelled request allowed: %+v", d)
	}
}

func TestApproveFDCancelledRequestLeavesTheApproverUsable(t *testing.T) {
	read, write := rawPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)

	ctx, cancel := context.WithCancel(context.Background())
	first := pendingApproval(t, ap, log, ctx, ui.Request{ID: "one", Reason: "r"})
	cancel()
	if d := receiveDecision(t, first, time.Second); d.Allow {
		t.Fatalf("cancelled request allowed: %+v", d)
	}

	second := pendingApproval(t, ap, log, context.Background(), ui.Request{ID: "two", Reason: "r"})
	if _, err := write.WriteString(`{"approval_id":"two","allow":true,"reason":"ok"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if d := receiveDecision(t, second, 2*time.Second); !d.Allow {
		t.Fatalf("approval after a cancel was not answered: %+v", d)
	}
}

// run.go ends the turn on SIGTERM by cancelling the context the approver is
// asked with. The signal arrives while an approval waits on a blocking
// descriptor the host never answers.
func TestApproveFDSIGTERMEndsAPendingApproval(t *testing.T) {
	read, _ := rawPipe(t)
	log := &approvalEvents{}
	ap := newFDApprover(read, log)

	ctx, cancel := runContext(time.Minute)
	defer cancel()
	done := pendingApproval(t, ap, log, ctx, ui.Request{ID: "t1", Reason: "x"})

	start := time.Now()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if d := receiveDecision(t, done, 2*time.Second); d.Allow {
		t.Fatalf("approval allowed on SIGTERM: %+v", d)
	}
	if ctx.Err() == nil {
		t.Fatal("SIGTERM did not cancel the run context")
	}
	t.Logf("SIGTERM to refusal in %s", time.Since(start).Round(time.Millisecond))
}

// descriptorShape is how a host may hand --approve-fd its end.
type descriptorShape struct {
	name string
	// open returns the descriptor manvi will be given, and what to close.
	open func(t *testing.T) *os.File
}

func approveFDShapes() []descriptorShape {
	return []descriptorShape{
		{"pipe read end", func(t *testing.T) *os.File {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = w.Close(); _ = r.Close() })
			return r
		}},
		{"fifo opened read-write", func(t *testing.T) *os.File {
			fifo := filepath.Join(t.TempDir(), "approvals")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Fatal(err)
			}
			// #nosec G304 -- fifo is a path this test just made under t.TempDir.
			f, err := os.OpenFile(fifo, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = f.Close() })
			return f
		}},
		{"socketpair end", func(t *testing.T) *os.File {
			fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
			if err != nil {
				t.Fatal(err)
			}
			host := os.NewFile(uintptr(fds[0]), "host-end")
			mine := os.NewFile(uintptr(fds[1]), "approve-fd")
			t.Cleanup(func() { _ = host.Close(); _ = mine.Close() })
			return mine
		}},
	}
}
