package credentials

import (
	"errors"
	"testing"
	"time"
)

// shortenMemguardBound runs a test with a short watchdog and restores it.
func shortenMemguardBound(t *testing.T, d time.Duration) {
	t.Helper()
	if n := memguardStuck.Load(); n != 0 {
		t.Fatalf("a previous test left %d stuck memguard call(s)", n)
	}
	saved := memguardBound
	memguardBound = d
	t.Cleanup(func() { memguardBound = saved })
}

// TestAMemguardCallThatNeverReturnsIsReportedNotWaitedFor covers the window
// the probe cannot close: locked memory taken by another process between the
// probe and memguard's own mlock, which deadlocks memguard on its global
// locks. The caller must get ErrLockedMemory within the bound, later callers
// must be refused at once instead of queueing behind the stuck one, and a call
// that does eventually return must have its result released and the refusal
// lifted.
func TestAMemguardCallThatNeverReturnsIsReportedNotWaitedFor(t *testing.T) {
	shortenMemguardBound(t, 50*time.Millisecond)

	release := make(chan struct{})
	discarded := make(chan int, 1)
	start := time.Now()
	_, err := guarded(1, func() int { <-release; return 7 }, func(v int) { discarded <- v })
	if !errors.Is(err, ErrLockedMemory) {
		t.Fatalf("err = %v, want ErrLockedMemory", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the stuck call was waited on for %s", elapsed)
	}
	if n := memguardStuck.Load(); n != 1 {
		t.Fatalf("stuck = %d, want 1", n)
	}

	ran := false
	start = time.Now()
	if _, err := guarded(1, func() int { ran = true; return 1 }, nil); !errors.Is(err, ErrLockedMemory) {
		t.Fatalf("a call behind a stuck one returned %v, want ErrLockedMemory", err)
	}
	if ran {
		t.Fatal("a call behind a stuck one reached memguard")
	}
	if elapsed := time.Since(start); elapsed > memguardBound {
		t.Fatalf("a call behind a stuck one took %s; it must be refused at once", elapsed)
	}

	close(release)
	select {
	case v := <-discarded:
		if v != 7 {
			t.Fatalf("discarded %d, want the late result 7", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the late result was never released")
	}

	memguardBound = 5 * time.Second
	got, err := guarded(1, func() int { return 9 }, nil)
	if err != nil || got != 9 {
		t.Fatalf("after the stuck call returned: %d, %v; want 9 and no error", got, err)
	}
	if n := memguardStuck.Load(); n != 0 {
		t.Fatalf("stuck = %d after the call returned, want 0", n)
	}
}

// TestAMemguardPanicIsNotTurnedIntoAnError: a panic means memguard completed a
// Purge, which destroyed every guarded buffer in the process. Reporting that
// as an ordinary error would let the harness carry on with a scrubber that
// scrubs nothing, so it is re-raised on the caller's goroutine, where it was
// raised before guarded existed.
func TestAMemguardPanicIsNotTurnedIntoAnError(t *testing.T) {
	shortenMemguardBound(t, 5*time.Second)

	func() {
		defer func() {
			if p := recover(); p != "purged" {
				t.Fatalf("recovered %v, want the panic re-raised", p)
			}
		}()
		_, _ = guarded(1, func() int { panic("purged") }, nil)
		t.Fatal("guarded returned after its call panicked")
	}()

	if got, err := guarded(1, func() int { return 3 }, nil); err != nil || got != 3 {
		t.Fatalf("the slot was not released after a panic: %d, %v", got, err)
	}
}
