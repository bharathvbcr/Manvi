//go:build unix

package main

import (
	"syscall"
	"testing"
)

// mkfifo makes a named pipe at path, or skips the test when the filesystem
// cannot hold one.
func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
}
