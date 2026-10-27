//go:build windows

package filelock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// LockFile takes an exclusive lock on the open file f, waiting until ctx ends.
// It takes ownership of f: the returned unlock releases the lock and closes it,
// and a failure closes it too.
//
// LockFileEx is the Windows counterpart of flock: the lock belongs to the
// handle and is released when the holder exits, so a crashed session cannot
// wedge later ones. As on Unix, the lock file is never removed.
func LockFile(ctx context.Context, f *os.File) (func(), error) {
	h := windows.Handle(f.Fd())
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(h, 0, 1, 0, ol)
				_ = f.Close()
			}, nil
		}
		// The lock was never taken, so the handle holds nothing a failed close
		// could lose; the error worth returning is the one that stopped us.
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
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

// Lock takes an exclusive lock on path, creating it if need be and waiting
// until ctx ends. It is LockFile on a file Lock opens.
func Lock(ctx context.Context, path string) (func(), error) {
	// #nosec G304 G703 -- the path is chosen by the calling package, never by a request.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return LockFile(ctx, f)
}
