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
	// #nosec G304 G703 -- the validated ledger path plus ".lock"; the operator chose it.
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
		// The lock was never taken, so the handle holds nothing a failed close
		// could lose; the error worth returning is the one that stopped us.
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, fmt.Errorf("budget: lock %s: %w", path, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// The decision constants live in liveness.go so every platform can test the
// rule; these fail the build if they ever stop matching the Windows values.
var (
	_ [0]struct{} = [waitObject0 - windows.WAIT_OBJECT_0]struct{}{}
	_ [0]struct{} = [waitTimeout - int(windows.WAIT_TIMEOUT)]struct{}{}
	_ [0]struct{} = [stillActive - int(windows.STATUS_PENDING)]struct{}{}
)

// processGone reports whether pid names no running process. Only a definite
// answer counts: a process Windows will not let us open (access denied) is
// treated as alive, so the attempt it owns is left alone.
//
// GetExitCodeProcess reports STILL_ACTIVE (259) for a running process and also
// for one that exited with code 259, so on that value the handle is asked
// directly: a process object is signaled once the process has exited. That wait
// needs SYNCHRONIZE access. If Windows grants the process handle without it,
// the probe falls back to the exit code alone and 259 stays "alive".
func processGone(pid int) bool {
	if pid <= 0 {
		return true
	}
	canWait := true
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		canWait = false
		h, err = windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	}
	if err != nil {
		return exitProbe{OpenErr: err, NoSuchProcess: errors.Is(err, windows.ERROR_INVALID_PARAMETER)}.gone()
	}
	defer windows.CloseHandle(h)

	probe := exitProbe{}
	probe.ExitCodeErr = windows.GetExitCodeProcess(h, &probe.ExitCode)
	if canWait {
		probe.Wait = func() (uint32, error) { return windows.WaitForSingleObject(h, 0) }
	}
	return probe.gone()
}
