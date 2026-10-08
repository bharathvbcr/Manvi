//go:build unix

package devcouncil

import "syscall"

// runMkfifo builds a fifo for the tests that prove a non-regular file is
// refused. Mkfifo is not in the portable syscall surface, so it lives behind a
// build tag; callers skip when it reports an error.
func runMkfifo(path string, mode uint32) error {
	return syscall.Mkfifo(path, mode)
}
