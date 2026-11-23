//go:build !unix

package main

import "testing"

// unblockPipe is a no-op where a named pipe cannot be created at a marker path.
func unblockPipe(string) {}

// mkfifo skips: there is no named pipe to put at a marker path on this platform.
func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("named pipes cannot be created here")
}
