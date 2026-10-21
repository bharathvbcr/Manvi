//go:build windows

package codingagent

import "os"

// openNonBlocking is a plain open: Windows has no FIFOs, and the regular-file
// check that precedes it already refuses devices.
func openNonBlocking(path string) (*os.File, error) {
	// #nosec G304 -- the operator-configured provider executable, already
	// resolved and inspected by the caller; only its header is read.
	return os.Open(path)
}

// executableWithoutMode is true because Windows file modes carry no execute
// bit; what makes a file runnable there is its PE image, checked next.
func executableWithoutMode() bool { return true }
