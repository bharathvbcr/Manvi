package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// heldClock is a StallClock whose timers run only when the test says so, and
// whose Stop reports that the callback has already started. That is the window
// ResponseHeaderTimeout's bound has to survive: the runtime has committed to
// running the callback, so Stop returns false, while Do has already produced
// the response. No sleep reaches it reliably, which is why the clock is held.
type heldClock struct {
	mu        sync.Mutex
	callbacks []func()
	// stopWins makes Stop report that it prevented the callback, which is the
	// other ordering: the callback is queued but has not started.
	stopWins bool
}

func (*heldClock) Now() time.Time { return time.Unix(1<<30, 0) }

func (c *heldClock) AfterFunc(_ time.Duration, f func()) StallTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callbacks = append(c.callbacks, f)
	return heldTimer{stopWins: c.stopWins}
}

func (c *heldClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.callbacks)
}

// fire runs every armed callback, as the runtime would once the delay passed.
func (c *heldClock) fire() {
	c.mu.Lock()
	callbacks := append([]func(){}, c.callbacks...)
	c.mu.Unlock()
	for _, f := range callbacks {
		f()
	}
}

type heldTimer struct{ stopWins bool }

func (heldTimer) Reset(time.Duration) bool { return false }
func (t heldTimer) Stop() bool             { return t.stopWins }

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ctxBody reads like a network body bound to its request: once the request's
// context is cancelled the read fails, as net/http's does.
type ctxBody struct {
	ctx context.Context
	r   io.Reader
}

func (b ctxBody) Read(p []byte) (int, error) {
	if err := context.Cause(b.ctx); err != nil {
		return 0, err
	}
	return b.r.Read(p)
}

func (ctxBody) Close() error { return nil }

func heldHeaderClient(clock StallClock, rt http.RoundTripper) *Client {
	return &Client{
		Provider:              "p",
		BaseURL:               "http://example.invalid",
		HTTP:                  &http.Client{Transport: rt},
		ResponseHeaderTimeout: time.Minute,
		clock:                 clock,
	}
}

func headerRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://example.invalid/x", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestAResponseThatBeatsTheHeaderTimerIsKept. The server answered; the timer's
// callback was already committed to running when Do returned, so Stop reported
// false. That is a successful response, not a timeout: send used to read the
// false as "the timer won", close the body, and report ErrHeaderTimeout, so
// Post discarded a 200 and retried it. And the callback that then ran cancelled
// the request context, killing the body of a response that had been handed back.
func TestAResponseThatBeatsTheHeaderTimerIsKept(t *testing.T) {
	t.Run("callback already started", func(t *testing.T) { responseBeatsTheTimer(t, false) })
	t.Run("callback still queued", func(t *testing.T) { responseBeatsTheTimer(t, true) })
}

func responseBeatsTheTimer(t *testing.T, stopWins bool) {
	t.Helper()
	clock := &heldClock{stopWins: stopWins}
	c := heldHeaderClient(clock, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       ctxBody{ctx: r.Context(), r: strings.NewReader("data: x\n\n")},
			Request:    r,
		}, nil
	}))

	resp, err := c.send(context.Background(), headerRequest(t))
	if err != nil {
		var timeout *ErrHeaderTimeout
		if errors.As(err, &timeout) {
			t.Fatalf("a 200 that arrived in the timer's window was discarded as a header timeout: %v", err)
		}
		t.Fatal(err)
	}
	if resp == nil {
		t.Fatal("send returned neither a response nor an error: the 200 was discarded and nothing was reported")
	}
	defer func() { _ = resp.Body.Close() }()
	if n := clock.armed(); n != 1 {
		t.Fatalf("armed %d timers on the test's clock, want 1; the bound is not running on it", n)
	}

	// The callback the runtime had already committed to now runs, late.
	clock.fire()

	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "data: x\n\n" {
		t.Fatalf("body=%q err=%v; a late timer callback cancelled a response that had been returned", body, err)
	}
}

// TestAMuteRoundTripIsAHeaderTimeout keeps the other side of the claim: a Do
// that has returned nothing when the timer fires is the timeout, and it is
// reported as one.
func TestAMuteRoundTripIsAHeaderTimeout(t *testing.T) {
	clock := &heldClock{}
	c := heldHeaderClient(clock, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		clock.fire()
		<-r.Context().Done()
		return nil, context.Cause(r.Context())
	}))

	resp, err := c.send(context.Background(), headerRequest(t))
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a mute round trip answered")
	}
	var timeout *ErrHeaderTimeout
	if !errors.As(err, &timeout) || timeout.After != time.Minute {
		t.Fatalf("err = %v, want an ErrHeaderTimeout after the configured minute", err)
	}
}

// TestAZeroHeaderTimeoutArmsNothing: zero disables the bound, and a negative
// value is not a duration to wait for.
func TestAZeroHeaderTimeoutArmsNothing(t *testing.T) {
	for _, limit := range []time.Duration{0, -time.Second} {
		clock := &heldClock{}
		c := heldHeaderClient(clock, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Request: r}, nil
		}))
		c.ResponseHeaderTimeout = limit
		resp, err := c.send(context.Background(), headerRequest(t))
		if err != nil {
			t.Fatalf("limit %s: %v", limit, err)
		}
		_ = resp.Body.Close()
		if n := clock.armed(); n != 0 {
			t.Fatalf("limit %s armed %d timers, want none", limit, n)
		}
	}
}
