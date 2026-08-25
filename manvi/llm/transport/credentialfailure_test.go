package transport

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestACredentialFailureIsNotRetried.
//
// attempt() has always said so in a comment — "a credential that cannot be
// resolved is not a transient failure — retrying it just delays the same
// error" — and the classification did not honour it. The error it built carried
// Status 0, and Retryable() reads Status 0 as "never reached a server, probably
// a dropped dial", so the resolver was called four times and backed off between
// each. On a turn with a deadline the loop then ran out of time and reported
// "context deadline exceeded": the timer named instead of the cause, sending an
// operator to look for a network problem that was not there.
func TestACredentialFailureIsNotRetried(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
	}))
	defer srv.Close()

	var resolved atomic.Int32
	c := New("test", srv.URL, func() (http.Header, error) {
		resolved.Add(1)
		return nil, errors.New("refusing to send this key to that host")
	})
	// Zero delay so a build that does retry fails on the count rather than on
	// the wall clock.
	c.Retry = RetryPolicy{MaxAttempts: 4, BaseDelay: 0, MaxDelay: 0}

	_, err := c.Post(context.Background(), "/v1/x", map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("a request with an unresolvable credential succeeded")
	}
	if !strings.Contains(err.Error(), "refusing to send this key to that host") {
		t.Errorf("error = %v; it does not carry what the resolver said", err)
	}
	if strings.Contains(err.Error(), "attempts") {
		t.Errorf("error = %v; a permanent failure was reported as an exhausted retry budget", err)
	}
	if got := resolved.Load(); got != 1 {
		t.Errorf("the credential was resolved %d times, want 1 — the answer cannot change", got)
	}
	if got := served.Load(); got != 0 {
		t.Errorf("the server saw %d request(s); a request whose credential was refused must not be sent", got)
	}

	var failure *Error
	if !errors.As(err, &failure) || failure.Retryable() {
		t.Errorf("error = %v; a credential failure must classify as non-retryable, "+
			"because callers decide provider fail-over on that answer", err)
	}
}

// TestARefusedDialIsStillRetried is the control on the same predicate. Status 0
// covers both "the credential was refused here" and "the dial failed", and only
// the first is permanent — collapsing them in the other direction would stop
// the transport retrying the most retryable fault there is.
func TestARefusedDialIsStillRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now, so every dial is refused

	c := New("test", url, func() (http.Header, error) { return http.Header{}, nil })
	c.Retry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 2 * time.Millisecond}

	_, err := c.Post(context.Background(), "/v1/x", map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("a dial to a closed port succeeded")
	}
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error = %v, want a *transport.Error", err)
	}
	if failure.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 — a refused dial is transient and must be retried",
			failure.Attempts)
	}
}
