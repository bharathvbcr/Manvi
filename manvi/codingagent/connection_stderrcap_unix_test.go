//go:build unix

package codingagent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A stderr flood from a real child ends the connection with the cap as its
// reason. The reader used to see only io.EOF in most runs (19 of 20 on the
// reviewed tree): the process is killed, stdout closes, and the end-of-stream
// reached the caller ahead of the recorded cause.
func TestStderrCapOnARealChildReportsTheCapNotEOF(t *testing.T) {
	const flood = `while :; do echo flood flood flood flood flood flood flood flood flood flood >&2; done`
	for i := 0; i < 20; i++ {
		c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", flood}, t.TempDir(), func([]byte) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		// Nobody reads until the child is reaped and its stdout has ended, as when a
		// send that returned nil leaves the failure to the next read. A reader that
		// is already waiting wakes on the cancellation and sees the cause, which is
		// why the failure was rare in a test that read at once. The settle only
		// narrows the window: a reader still running just reads the cancellation.
		select {
		case <-c.done:
		case <-time.After(30 * time.Second):
			t.Fatalf("run %d: the flooded child was never reaped", i)
		}
		time.Sleep(100 * time.Millisecond)
		ctx, stop := context.WithTimeout(t.Context(), 30*time.Second)
		_, _, err = c.receiveFrame(ctx)
		stop()
		if _, closeErr := c.close(); closeErr != nil {
			t.Fatalf("run %d: close: %v", i, closeErr)
		}
		if !errors.Is(err, errStderrCap) {
			t.Fatalf("run %d: the flooded child's connection reported %v, want %v", i, err, errStderrCap)
		}
	}
}
