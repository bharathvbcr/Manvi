//go:build windows

package artifacts

import (
	"errors"
	"golang.org/x/sys/windows"
)

// lockHeldElsewhere reports an error from creating the lock file that means the
// name is taken for a moment, not that this process may not make it. Windows does
// not answer "exists" for every such case: a lock file that another writer has
// just deleted stays in the directory as a delete-pending entry until the last
// handle on it closes, and creating that name meanwhile fails with
// ERROR_ACCESS_DENIED; one that is open without sharing fails with
// ERROR_SHARING_VIOLATION. Both end when the other writer is done, which is what
// the bounded wait in lockEntry is for.
func lockHeldElsewhere(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION)
}
