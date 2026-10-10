//go:build windows

package budget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on path, waiting until ctx ends.
//
// LockFileEx is the Windows counterpart of flock: the lock belongs to the
// handle and is released when the holder exits, so a crashed session cannot
// wedge later ones. As on Unix, the lock file is never removed.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
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
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
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

// syncDir is a no-op on Windows. A directory handle cannot be flushed there
// (FlushFileBuffers needs write access a directory handle does not have), and
// NTFS journals the rename's metadata itself. Calling the Unix version would
// fail every ledger write.
func syncDir(string) error { return nil }

// processGone reports whether pid names no running process. Only a definite
// answer counts: a process Windows will not let us open (access denied) is
// treated as alive, so the attempt it owns is left alone.
func processGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_INVALID_PARAMETER)
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code != stillActive
}
