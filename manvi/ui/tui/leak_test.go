package tui

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestCaptureStderrRetiresItsGoroutine asserts the drain does not outlive the
// capture. The drain owns a pipe and a send on r.actions; leaking one per run
// would accumulate a blocked goroutine and an open descriptor for every session
// the process opens.
func TestCaptureStderrRetiresItsGoroutine(t *testing.T) {
	settle(t)
	before := runtime.NumGoroutine()

	for i := 0; i < 20; i++ {
		r := newCaptureRunner()
		restore := r.captureStderr()
		fmt.Fprintln(os.Stderr, "a diagnostic")
		// An oversized line too, since that is the path that used to end the
		// drain early and leave the pipe undrained.
		fmt.Fprintln(os.Stderr, strings.Repeat("q", 200_000))
		restore()
	}

	settle(t)
	after := runtime.NumGoroutine()
	// A little slack for runtime bookkeeping; a leak of one per iteration would
	// be twenty.
	if after > before+5 {
		t.Fatalf("goroutines grew from %d to %d across 20 captures", before, after)
	}
}

// settle waits for goroutines scheduled to exit to actually do so.
func settle(t *testing.T) {
	t.Helper()
	for i := 0; i < 50; i++ {
		runtime.GC()
		time.Sleep(10 * time.Millisecond)
	}
}
