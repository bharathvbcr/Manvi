//go:build !unix

package main

import "testing"

// mkfifo skips: a named pipe cannot be made here.
func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("named pipes are not available on this platform")
}
