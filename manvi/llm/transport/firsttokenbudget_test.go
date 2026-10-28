package transport

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestTheFirstTokenBudgetIsPinnedForLocalAndHostedAlike. Hosted streams get the
// local 15 minutes deliberately; FirstTokenBudget says why. This pins the
// numbers so lowering one is a decision made against a measurement, not a drive-by.
func TestTheFirstTokenBudgetIsPinnedForLocalAndHostedAlike(t *testing.T) {
	const want = 15 * time.Minute
	if DefaultFirstTokenTimeout != want {
		t.Fatalf("DefaultFirstTokenTimeout = %s, want %s", DefaultFirstTokenTimeout, want)
	}
	if got := FirstTokenBudget(DefaultHostedStallTimeout); got != want {
		t.Fatalf("hosted first-token budget = %s, want %s", got, want)
	}
	if got := New("p", "http://127.0.0.1:8080/v1", nil).ResponseHeaderTimeout; got != want {
		t.Fatalf("loopback header bound = %s, want the first-token budget %s", got, want)
	}
	// The hosted bound on headers is a different quantity and stays short.
	if got := New("p", "https://api.example.com/v1", nil).ResponseHeaderTimeout; got != DefaultHostedHeaderTimeout {
		t.Fatalf("hosted header bound = %s, want %s", got, DefaultHostedHeaderTimeout)
	}
	// An operator's longer idle limit is never given a shorter first-token wait,
	// and a disabled watchdog disables the wait.
	if got := FirstTokenBudget(time.Hour); got != time.Hour {
		t.Fatalf("budget for a 1h idle limit = %s, want 1h", got)
	}
	if got := FirstTokenBudget(0); got != 0 {
		t.Fatalf("budget with the watchdog disabled = %s, want unbounded (0)", got)
	}
}

// TestPingsDoNotRestartTheFirstOutputWait. The preflight's budget is one timer
// over the whole inspection: a stream of nothing but preamble frames (pings, a
// message_start) is not producing output however often it writes, and it must
// not be allowed to hold the call open past the budget.
func TestPingsDoNotRestartTheFirstOutputWait(t *testing.T) {
	pr, pw := io.Pipe()
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
				if _, err := io.WriteString(pw, "event: ping\ndata: {\"type\": \"ping\"}\n\n"); err != nil {
					return
				}
			}
		}
	}()
	defer func() {
		close(stop)
		_ = pr.Close()
		wg.Wait()
	}()

	start := time.Now()
	_, failure := PreflightStream(&http.Response{Body: pr}, "p", 80*time.Millisecond,
		func(string) (bool, *Error) { return true, nil })
	if failure == nil || !failure.Retryable() {
		t.Fatalf("failure = %v, want a retryable stall", failure)
	}
	var stalled *ErrStalled
	if !errors.As(failure, &stalled) || !stalled.BeforeFirstToken {
		t.Fatalf("failure = %v, want an ErrStalled before the first token", failure)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s; pings kept the wait open", d)
	}
}
