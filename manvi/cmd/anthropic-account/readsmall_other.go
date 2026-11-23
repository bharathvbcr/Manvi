//go:build !unix

package main

import (
	"fmt"
	"os"
)

// openMarker is the platform without O_NOFOLLOW (Windows). It refuses anything
// whose own directory entry is not a plain regular file: Lstat does not follow
// a link, and on Windows it reports symbolic links, junctions and other reparse
// points as non-regular modes, so every redirect is refused rather than
// honoured. This fails closed: a mode this code does not recognise is a
// refusal, never a pass. A pipe cannot be created at a marker path here, and
// the caller still verifies the opened handle is a regular file. The window
// between Lstat and open is the one O_NOFOLLOW closes on Unix and cannot be
// closed here without a Windows-specific open; the marker only selects an
// account name, and the opened handle's type is re-checked.
func openMarker(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symbolic link; the account marker must be a regular file where it sits", path)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file (%s)", path, info.Mode().Type())
	}
	// #nosec G304 -- path is the marker location the launcher walked to.
	return os.Open(path)
}
