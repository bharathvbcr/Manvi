package transport

import (
	"testing"
	"time"
)

// TestRetryAfterCarriesJitterOnTop. A server that rate-limits many clients at
// once answers them all with the same Retry-After, and a client that waits
// exactly that long returns exactly together. The jitter is added above the
// server's time and never subtracts from it: an earlier retry is the one thing
// Retry-After exists to prevent.
func TestRetryAfterCarriesJitterOnTop(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 8 * time.Second, Jitter: 0.2}
	const server = 10 * time.Second
	draw := func(v float64) func() float64 { return func() float64 { return v } }

	if got := p.delayFor(1, server, draw(0)); got != server {
		t.Fatalf("lowest draw: delay = %v, want exactly the server's %v", got, server)
	}
	high := p.delayFor(1, server, draw(0.999999))
	if high <= server || high > server+time.Duration(0.2*float64(server)) {
		t.Fatalf("highest draw: delay = %v, want above %v and within the 20%% spread", high, server)
	}
	for _, v := range []float64{0, 0.001, 0.25, 0.5, 0.75, 0.999999} {
		if got := p.delayFor(1, server, draw(v)); got < server {
			t.Fatalf("draw %v: delay = %v, before the server's %v", v, got, server)
		}
	}
	if a, b := p.delayFor(1, server, draw(0.1)), p.delayFor(1, server, draw(0.9)); a == b {
		t.Fatalf("different draws gave the same delay %v; clients that failed together still retry together", a)
	}
}

// TestRetryAfterJitterIsOptional: no draw source or no jitter fraction means
// the server's time, verbatim, which is what the exact-sequence tests rely on.
func TestRetryAfterJitterIsOptional(t *testing.T) {
	const server = 3 * time.Second
	draw := func() float64 { return 0.999 }
	with := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 8 * time.Second, Jitter: 0.2}
	if got := with.delayFor(1, server, nil); got != server {
		t.Fatalf("nil draw: delay = %v, want %v", got, server)
	}
	without := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 8 * time.Second}
	if got := without.delayFor(1, server, draw); got != server {
		t.Fatalf("zero Jitter: delay = %v, want %v", got, server)
	}
}

// TestRetryAfterJitterIsSpreadAboveTheCeiling. MaxRetryAfter bounds what a
// server can demand, not the spread added to it. Clipping the sum back to the
// ceiling would put every client that was told something larger at the same
// instant again — the herd the jitter is there to break — so the jitter rides
// on the clamped value and the longest wait is ceiling plus its spread, no more:
// a hostile header cannot multiply it.
func TestRetryAfterJitterIsSpreadAboveTheCeiling(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 8 * time.Second, Jitter: 0.2, MaxRetryAfter: 30 * time.Second}
	draw := func(v float64) func() float64 { return func() float64 { return v } }

	if got := p.delayFor(1, 24*time.Hour, draw(0)); got != 30*time.Second {
		t.Fatalf("lowest draw above the ceiling: delay = %v, want the ceiling", got)
	}
	got := p.delayFor(1, 24*time.Hour, draw(0.999999))
	if got <= 30*time.Second || got > 36*time.Second {
		t.Fatalf("highest draw above the ceiling: delay = %v, want within (30s, 36s]", got)
	}
	if a, b := p.delayFor(1, 24*time.Hour, draw(0.1)), p.delayFor(1, time.Hour, draw(0.9)); a == b {
		t.Fatalf("clients told different oversized values waited the same %v", a)
	}
}
