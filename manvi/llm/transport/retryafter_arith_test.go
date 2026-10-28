package transport

import (
	"math"
	"strconv"
	"testing"
	"time"
)

func nearOne() float64 { return math.Nextafter(1, 0) }

// TestJitterIsClampedToItsDocumentedRange. Jitter is a fraction in [0,1]. A
// value outside it used to be taken at face value: 5 stretched a one-hour
// Retry-After to five hours past the ceiling's spread, and a negative one
// shortened a server's time, which is the one thing Retry-After forbids.
func TestJitterIsClampedToItsDocumentedRange(t *testing.T) {
	const server = 10 * time.Second
	p := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 8 * time.Second}

	p.Jitter = 5
	if got := p.delayFor(1, server, nearOne); got < server || got > 2*server {
		t.Fatalf("Jitter 5: Retry-After %v -> %v, want within [%v, %v]", server, got, server, 2*server)
	}
	p.Jitter = -1
	if got := p.delayFor(1, server, nearOne); got != server {
		t.Fatalf("Jitter -1: Retry-After %v -> %v, want exactly the server's time", server, got)
	}
	// The computed backoff is bounded the same way: a Jitter of 5 spread it
	// from -1.5x to +3.5x of itself, and a negative duration is not a wait.
	p.Jitter = 5
	for _, draw := range []float64{0, 0.5, nearOne()} {
		d := draw
		if got := p.delayFor(1, 0, func() float64 { return d }); got < 0 || got > 2*time.Second {
			t.Fatalf("Jitter 5 backoff, draw %v: %v, want within [0, 2s]", draw, got)
		}
	}
}

// TestRetryAfterArithmeticSaturates. The jitter is added to a duration the
// policy lets the operator set arbitrarily high, and the sum wrapped to a
// negative one — an instant retry where a very long wait was asked for.
func TestRetryAfterArithmeticSaturates(t *testing.T) {
	huge := time.Duration(math.MaxInt64)
	p := DefaultRetryPolicy()
	p.MaxRetryAfter = huge
	for _, draw := range []float64{0, 0.5, nearOne()} {
		d := draw
		got := p.delayFor(1, huge, func() float64 { return d })
		if got <= 0 {
			t.Fatalf("draw %v: Retry-After at the duration limit gave %v, want a saturated wait", draw, got)
		}
	}
	if got := p.delayFor(1, huge-1, nearOne); got != huge {
		t.Fatalf("an addition past the limit gave %v, want it to saturate at %v", got, huge)
	}
}

// TestAHugeRetryAfterIsNotAShortBackoff. delay-seconds was multiplied by
// time.Second unchecked, so 9223372037 seconds wrapped negative and the
// header that asked for the longest wait produced no wait at all; a value
// beyond int64 failed to parse and was treated as absent. Both now saturate and
// meet the ceiling like any other oversized header.
func TestAHugeRetryAfterIsNotAShortBackoff(t *testing.T) {
	for _, hint := range []string{
		"9223372036", "9223372037", "18446744073", "18446744074",
		strconv.FormatInt(math.MaxInt64, 10), "9223372036854775808", "99999999999999999999999",
	} {
		got := retryAfter(&Error{retryHint: hint})
		if got < time.Hour {
			t.Errorf("Retry-After %s -> %v, want a very long wait", hint, got)
			continue
		}
		p := DefaultRetryPolicy()
		if d := p.delayFor(1, got, nearOne); d < p.MaxRetryAfter {
			t.Errorf("Retry-After %s -> backoff %v, shorter than the %v ceiling", hint, d, p.MaxRetryAfter)
		}
	}
	// Ordinary and malformed values are untouched.
	for hint, want := range map[string]time.Duration{"0": 0, "3": 3 * time.Second, " 7 ": 7 * time.Second, "-5": 0, "soon": 0} {
		if got := retryAfter(&Error{retryHint: hint}); got != want {
			t.Errorf("Retry-After %q -> %v, want %v", hint, got, want)
		}
	}
}
