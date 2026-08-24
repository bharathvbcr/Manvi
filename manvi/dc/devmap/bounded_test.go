package devmap

import (
	"context"
	"testing"
	"time"
)

// The bound has to cover the fork, not just the process.
//
// The helper itself lives in manvi/dc/internal/proc, with its own tests, because
// three subprocess boundaries need it and only one of them had it. What is left
// here is the invariant at the level devmap's callers use.
//
// Before this, decode called `cmd.Run()` directly and relied on
// exec.CommandContext plus Cmd.WaitDelay. Both of those start counting after
// Start has returned, so a Start that does not return is outside every bound
// the function advertises. Running `./verify.sh --race` reproduced exactly
// that: a goroutine parked in `c.Start()` for the full 900-second package
// timeout with its 30-second context untouched.
//
// These tests state the invariant directly, because the end-to-end wedge needs
// a loaded machine and the race detector to reproduce, and a regression test
// that only fails on a bad day is not one.

// TestDecodeRefusesWhenItsDeadlineHasAlreadyPassed is the same invariant at the
// level callers use, without needing a wedged fork to produce it.
func TestDecodeRefusesWhenItsDeadlineHasAlreadyPassed(t *testing.T) {
	c := fake(t, map[string]string{"status": healthyStatus})

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // the deadline is now certainly past

	start := time.Now()
	_, err := c.Status(ctx)
	if err == nil {
		t.Fatal("a query whose deadline had already passed returned an answer")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s to refuse an expired deadline", elapsed)
	}
}
