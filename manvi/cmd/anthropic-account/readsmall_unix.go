//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openMarker opens the account marker read-only without following a symbolic
// link in its last component and without blocking on a named pipe. O_NOFOLLOW
// turns a link into ELOOP, which is reported as a refusal rather than a
// redirect to a file the repository does not own; O_NONBLOCK keeps an open of
// a pipe from waiting for a writer. The caller still checks that the opened
// descriptor is a regular file.
func openMarker(path string) (*os.File, error) {
	// #nosec G304 -- path is the marker location the launcher walked to.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, fmt.Errorf("%s is a symbolic link; the account marker must be a regular file where it sits", path)
	}
	return f, err
}
