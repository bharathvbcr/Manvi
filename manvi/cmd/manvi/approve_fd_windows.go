//go:build windows

package main

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// prepareApproveFD stops handle being inherited by any process this one starts.
//
// On Windows --approve-fd names a HANDLE, not a small descriptor number, and
// the equivalent of close-on-exec is clearing its inherit flag. The call fails
// on a value that is not an open handle, which is how a mistyped --approve-fd
// is told apart from a real one before anything is spawned.
func prepareApproveFD(handle int) error {
	if err := windows.SetHandleInformation(windows.Handle(handle), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fmt.Errorf("not an open handle, or its inherit flag could not be cleared: %w", err)
	}
	return nil
}
