package gussetcheck

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
	"time"
)

// withReady swaps the check Ready runs and resets its cache for one test.
func withReady(t *testing.T, run func(context.Context) error) {
	t.Helper()
	prevRun, prevRetry := runReady, readyRetry
	reset := func() {
		readyMu.Lock()
		readyOK, readyLatched, readyLast, readyAt = false, nil, nil, time.Time{}
		readyMu.Unlock()
	}
	reset()
	runReady = run
	t.Cleanup(func() {
		runReady, readyRetry = prevRun, prevRetry
		reset()
	})
}

// A first check that ran out of time was a sync.Once result: every later
// policy check was refused until restart. It is retried after readyRetry.
func TestReadyRetriesATransientFailure(t *testing.T) {
	for _, first := range []error{
		context.DeadlineExceeded,
		fmt.Errorf("open handle: %w", syscall.EMFILE),
	} {
		calls := 0
		withReady(t, func(context.Context) error {
			calls++
			if calls == 1 {
				return first
			}
			return nil
		})
		readyRetry = 20 * time.Millisecond
		if err := Ready(); !errors.Is(err, first) {
			t.Fatalf("first Ready = %v, want %v", err, first)
		}
		if err := Ready(); !errors.Is(err, first) || calls != 1 {
			t.Fatalf("Ready inside the retry window = %v after %d checks; want the cached failure", err, calls)
		}
		time.Sleep(30 * time.Millisecond)
		if err := Ready(); err != nil || calls != 2 {
			t.Fatalf("Ready after the retry window = %v after %d checks; want a pass on a second check", err, calls)
		}
		if err := Ready(); err != nil || calls != 2 {
			t.Fatalf("a pass was not kept: %v after %d checks", err, calls)
		}
	}
}

// A verdict stays: a poisoned engine or a parity mismatch does not start
// answering on a later call, however long it has been.
func TestReadyLatchesAVerdict(t *testing.T) {
	verdict := errors.New("gusset and Go fnmatch disagree")
	calls := 0
	withReady(t, func(context.Context) error { calls++; return verdict })
	readyRetry = time.Millisecond
	for i := 0; i < 3; i++ {
		if err := Ready(); !errors.Is(err, verdict) {
			t.Fatalf("Ready = %v, want the latched verdict", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if calls != 1 {
		t.Fatalf("a latched verdict re-ran the check %d times", calls)
	}
}

func TestReadyLatchesNotLinked(t *testing.T) {
	calls := 0
	withReady(t, func(context.Context) error { calls++; return ErrNotLinked })
	readyRetry = time.Millisecond
	_ = Ready()
	time.Sleep(2 * time.Millisecond)
	if err := Ready(); !errors.Is(err, ErrNotLinked) || calls != 1 {
		t.Fatalf("Ready = %v after %d checks; ErrNotLinked must stay without re-checking", err, calls)
	}
}
