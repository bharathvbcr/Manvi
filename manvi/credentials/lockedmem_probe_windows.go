//go:build windows

package credentials

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// workingSetStep is how far probeLockedMemory raises the process's minimum
// working set each time VirtualLock reports the quota exhausted, and
// workingSetCeiling is the most it will raise it to in total.
//
// Windows lets a process lock only about as many pages as its minimum working
// set holds, and the default minimum is 200 KiB: roughly fifty pages. A
// process that seals a few credentials and arms a scrubber over them reaches
// that, and memguard's answer to the failed VirtualLock is the deadlock this
// package's guarded exists to keep out of the process. Raising the minimum is
// the documented remedy (SetProcessWorkingSetSizeEx) and costs nothing until
// the pages are used. It never makes a credential unlocked: when the raise
// is refused, or the ceiling is reached, the probe fails and the credential is
// refused, as on every other platform.
const (
	workingSetStep    = 4 << 20
	workingSetCeiling = 256 << 20
)

// probeLockedMemory locks and releases pages pages of committed memory, which is
// the operation memguard is about to attempt. An error means memguard's would
// fail too. When the lock fails, the working-set quota is raised and the lock
// tried once more.
func probeLockedMemory(pages int) error {
	size := uintptr(pages * os.Getpagesize())
	addr, err := windows.VirtualAlloc(0, size, windows.MEM_COMMIT|windows.MEM_RESERVE, windows.PAGE_READWRITE)
	if err != nil {
		return fmt.Errorf("VirtualAlloc: %w", err)
	}
	lockErr := windows.VirtualLock(addr, size)
	if lockErr != nil {
		// Whatever the code, the one thing that can make a lock succeed on a
		// retry is more working set, and a refused or failed raise leaves the
		// first error standing.
		if growErr := growWorkingSet(size); growErr != nil {
			lockErr = fmt.Errorf("%w (raising the working set failed: %w)", lockErr, growErr)
		} else {
			lockErr = windows.VirtualLock(addr, size)
		}
	}
	if lockErr != nil {
		lockErr = fmt.Errorf("VirtualLock: %w", lockErr)
	} else if err := windows.VirtualUnlock(addr, size); err != nil {
		lockErr = fmt.Errorf("VirtualUnlock: %w", err)
	}
	if err := windows.VirtualFree(addr, 0, windows.MEM_RELEASE); err != nil {
		return errors.Join(lockErr, fmt.Errorf("VirtualFree: %w", err))
	}
	return lockErr
}

// growWorkingSet raises the process's minimum working set by enough to lock
// need more bytes, and its maximum with it.
func growWorkingSet(need uintptr) error {
	process := windows.CurrentProcess()
	var minSize, maxSize uintptr
	var flags uint32
	windows.GetProcessWorkingSetSizeEx(process, &minSize, &maxSize, &flags)
	grow := max(uintptr(workingSetStep), 2*need)
	if minSize+grow > workingSetCeiling {
		return fmt.Errorf("the minimum working set would pass %d MiB", workingSetCeiling>>20)
	}
	minSize += grow
	if maxSize < minSize+workingSetStep {
		maxSize = minSize + workingSetStep
	}
	if err := windows.SetProcessWorkingSetSizeEx(process, minSize, maxSize, 0); err != nil {
		return fmt.Errorf("SetProcessWorkingSetSizeEx: %w", err)
	}
	return nil
}
