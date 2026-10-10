//go:build unix

package budget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive advisory lock on path, waiting until ctx ends.
//
// flock rather than an O_EXCL marker file: the kernel drops a flock when its
// holder exits, so a crashed session cannot wedge every later one behind a
// stale lock. The lock file itself is never removed, because removing it lets
// two processes hold locks on two different inodes of the same name.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("budget: lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// syncDir makes a rename in dir durable. Without it a crash can bring back the
// previous ledger, and with it lose the record of money already spent.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// processGone reports whether pid names no running process. Only a definite
// "no such process" counts: EPERM means it exists under another user, and any
// other error is not proof, so the attempt it owns is left alone.
func processGone(pid int) bool {
	if pid <= 0 {
		// Never signal pid 0 or a negative pid: those address process groups.
		// No live process can own such an attempt.
		return true
	}
	return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}
