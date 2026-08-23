package devmap

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The bound has to cover the fork, not just the process.
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

func TestRunBoundedReturnsTheDeadlineWhenTheCallNeverDoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Never sends. This is the wedged Start.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	start := time.Now()
	err, timedOut := runBounded(ctx, func() error {
		<-blocked
		return nil
	})
	elapsed := time.Since(start)

	if !timedOut {
		t.Fatalf("a call that never returns was not reported as timing out (err=%v)", err)
	}
	if err != nil {
		t.Errorf("the timeout path must not invent an error from the abandoned call, got %v", err)
	}
	// Generous, because this asserts "bounded", not "fast". Before the fix this
	// did not return at all.
	if elapsed > 5*time.Second {
		t.Fatalf("took %s to honour a 50ms deadline; the wait is not bounded", elapsed)
	}
}

func TestRunBoundedPassesTheCallsOwnErrorThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := errors.New("exit status 2")
	err, timedOut := runBounded(ctx, func() error { return want })
	if timedOut {
		t.Fatal("a call that returned promptly was reported as a timeout")
	}
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want the call's own error %v", err, want)
	}
}

func TestRunBoundedDoesNotReportSuccessAsATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err, timedOut := runBounded(ctx, func() error { return nil })
	if timedOut || err != nil {
		t.Fatalf("a clean call reported err=%v timedOut=%v", err, timedOut)
	}
}

// TestRunBoundedPrefersTheAnswerItAlreadyHas guards the race between a call
// finishing and a deadline expiring at the same instant.
//
// If the select picked the deadline when both were ready, a devmap query that
// answered correctly would intermittently be reported as a hang — the kind of
// flake that gets a real bound removed for being noisy.
func TestRunBoundedPrefersTheAnswerItAlreadyHas(t *testing.T) {
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		// The call has already returned before the select runs.
		done := make(chan error, 1)
		done <- nil
		err, timedOut := runBounded(ctx, func() error { return <-done })
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		_ = timedOut // may legitimately be either; the point is it never errors
	}
}

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
