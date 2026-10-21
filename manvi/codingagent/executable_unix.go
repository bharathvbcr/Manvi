//go:build !windows

package codingagent

import (
	"os"
	"syscall"
)

// openNonBlocking opens for reading without waiting on a FIFO that replaced the
// file after it was inspected.
func openNonBlocking(path string) (*os.File, error) {
	// #nosec G304 -- the path is the operator-configured provider executable,
	// already resolved and inspected by the caller; only its header is read.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}

// executableWithoutMode reports whether a file can be executable although it
// carries no execute permission bit. On these platforms it cannot.
func executableWithoutMode() bool { return false }
