//go:build !windows

package main

import (
	"os"
	"syscall"
)

// openRunImage opens for reading without waiting for a writer. A plain open of
// a FIFO blocks until the other end opens, and an --image path is read while
// the arguments are parsed, before any --timeout context exists.
func openRunImage(path string) (*os.File, error) {
	// #nosec G304 -- the operator named this --image path; it is read and sniffed, never executed
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
