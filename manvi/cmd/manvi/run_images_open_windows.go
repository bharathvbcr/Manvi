//go:build windows

package main

import "os"

// openRunImage is a plain open: Windows has no FIFOs to block on, and the
// regular-file check that follows refuses devices.
func openRunImage(path string) (*os.File, error) {
	// #nosec G304 -- the operator named this --image path; it is read and sniffed, never executed
	return os.Open(path)
}
