//go:build unix

package budget

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// wedgedLedger is a ledger whose file lock is held by someone who will not
// release it (another process under SIGSTOP, an NFS stall), with lockWait
// shortened so the wait the callers are queued behind ends in three seconds
// rather than the real thirty.
func wedgedLedger(t *testing.T) *Ledger {
	t.Helper()
	old := lockWait
	lockWait = 3 * time.Second
	t.Cleanup(func() { lockWait = old })

	path := filepath.Join(t.TempDir(), "ledger.json")
	l, err := Open(context.Background(), path, 1_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := lockFile(context.Background(), path+".lock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	return l
}

// The in-process lock used to be a sync.Mutex taken before the context-aware
// file lock: while one goroutine waited out lockWait for a wedged file lock,
// every other goroutine in the process queued behind it for the full wait,
// including one whose context was already cancelled. A fan-out that was
// cancelled could not stop reserving, or release what it held, until the
// wedged holder's timeout had passed, once per waiter.
func TestACancelledCallerIsNotHeldByAnotherCallersWaitForTheFileLock(t *testing.T) {
	l := wedgedLedger(t)

	first := make(chan struct{})
	go func() { // a sibling goroutine already waiting for the file lock
		_, _ = l.Snapshot(context.Background())
		close(first)
	}()
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	done := make(chan error, 1)
	go func() { _, err := l.Snapshot(ctx); done <- err }()
	time.Sleep(100 * time.Millisecond)
	cancel() // the user hit Ctrl-C
	err := <-done
	waited := time.Since(started)
	t.Logf("a caller cancelled at 100ms returned after %s with %v", waited.Round(10*time.Millisecond), err)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if waited > time.Second {
		t.Fatalf("cancelled caller was held %s by another goroutine's lock wait", waited)
	}
	<-first
}

// A caller with no deadline of its own queued behind a wedged holder is
// bounded by lockWait in all, not by lockWait for the lock the holder is
// waiting on and again for its own turn after it.
func TestQueuedCallersShareOneLockWaitNotOneEach(t *testing.T) {
	l := wedgedLedger(t)

	first := make(chan struct{})
	go func() {
		_, _ = l.Snapshot(context.Background())
		close(first)
	}()
	time.Sleep(200 * time.Millisecond)

	started := time.Now()
	_, err := l.Snapshot(context.Background())
	waited := time.Since(started)
	t.Logf("a second caller returned after %s with %v", waited.Round(10*time.Millisecond), err)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the lock wait's deadline", err)
	}
	if limit := lockWait + time.Second; waited > limit {
		t.Fatalf("a queued caller waited %s, more than one lockWait (%s)", waited, lockWait)
	}
	<-first
}
