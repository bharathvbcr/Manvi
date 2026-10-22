//go:build !unix

package devcouncil

import "testing"

// mkfifo skips: a named pipe is a unix file type, and this platform cannot put
// one at a path, so there is no FIFO for the skills listing to refuse.
func mkfifo(t *testing.T, _ string) {
	t.Helper()
	t.Skip("FIFOs are a unix file type; this platform has no mkfifo")
}
