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
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // read-only, non-blocking open of a path the caller resolved and inspected
}

// executableWithoutMode reports whether a file can be executable although it
// carries no execute permission bit. On these platforms it cannot.
func executableWithoutMode() bool { return false }

// openNoFollowNonBlocking is openNonBlocking that also refuses a symlink at the
// final path component, for files the checkout controls.
func openNoFollowNonBlocking(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0) //nolint:gosec // read-only, non-blocking, no-follow open of a path the caller inspected
}
