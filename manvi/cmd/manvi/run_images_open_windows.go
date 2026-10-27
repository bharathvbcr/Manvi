//go:build windows

package main

import (
	"errors"
	"io/fs"
	"os"
	"time"
)

// openRunImage is a plain open: the by-name check has already refused anything
// that is not a regular file, so there is no pipe or device for it to wait on.
func openRunImage(path string) (*os.File, error) {
	// #nosec G304 -- the operator named this --image path; it is read and sniffed, never executed
	return os.Open(path)
}

// refuseImagePath refuses a Windows named pipe by its name. Inspecting or
// opening `\\.\pipe\x` connects to it, so the check cannot be left to os.Stat,
// which would be the unbounded wait this guards against.
func refuseImagePath(path string) error {
	if windowsPipePath(path) {
		return errors.New("a named pipe is not read on Windows; write the image to a file and name that")
	}
	return nil
}

// imageSourceAllowed is a regular file only. Windows has no /dev/fd or
// /dev/stdin, and a named pipe is refused by name above.
func imageSourceAllowed(mode fs.FileMode) bool { return mode.IsRegular() }

// readImagePipe is unreachable on Windows, where imageSourceAllowed refuses
// everything that is not a regular file; it refuses again rather than read.
func readImagePipe(*os.File, time.Duration) ([]byte, error) {
	return nil, errors.New("a pipe is not read on Windows")
}
