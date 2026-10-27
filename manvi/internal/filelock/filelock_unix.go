//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos

package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// LockFile takes an exclusive advisory lock on the open file f, waiting until
// ctx ends. It takes ownership of f: the returned unlock releases the lock and
// closes it, and a failure closes it too.
//
// flock rather than an O_EXCL marker file: the kernel drops a flock when its
// holder exits, so a crashed session cannot wedge every later one behind a
// stale lock. The lock file itself is never removed, because removing it lets
// two processes hold locks on two different inodes of the same name.
func LockFile(ctx context.Context, f *os.File) (func(), error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		// The lock was never taken, so the descriptor holds nothing a failed
		// close could lose; the error worth returning is the one that stopped us.
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("lock %s: %w", f.Name(), err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// Lock takes an exclusive advisory lock on path, creating it if need be and
// waiting until ctx ends. It is LockFile on a file Lock opens.
func Lock(ctx context.Context, path string) (func(), error) {
	// #nosec G304 G703 -- the path is chosen by the calling package, never by a request.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return LockFile(ctx, f)
}
