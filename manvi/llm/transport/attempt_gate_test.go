package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAttemptAdmissionBoundsRetriesAndAccountsEachSend(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "post", true: "stream"}[streaming], func(t *testing.T) {
			var sends atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1); w.WriteHeader(503) }))
			defer srv.Close()
			var outcomes []AttemptResult
			admissions := 0
			ctx := WithAttemptGate(context.Background(), func(ctx context.Context, a AttemptInfo) (func(AttemptResult) error, error) {
				admissions++
				if a.Number != admissions || a.Path != "/model" {
					t.Fatalf("bad attempt info %+v", a)
				}
				if admissions > 2 {
					return nil, errors.New("budget exhausted")
				}
				return func(r AttemptResult) error { outcomes = append(outcomes, r); return nil }, nil
			})
			c := fastClient(t, srv.URL)
			var err error
			if streaming {
				_, err = c.PostStream(ctx, "/model?private=not-forwarded", nil, nil)
			} else {
				_, err = c.Post(ctx, "/model?private=not-forwarded", nil)
			}
			if err == nil || sends.Load() != 2 || len(outcomes) != 2 {
				t.Fatalf("sends=%d outcomes=%v err=%v", sends.Load(), outcomes, err)
			}
			for _, out := range outcomes {
				if out.State != AttemptRejected {
					t.Fatalf("outcome=%+v", out)
				}
			}
		})
	}
}

func TestAttemptCanceledWhileAdmissionWaitsIsNotSent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var outcome AttemptResult
	ctx = WithAttemptGate(ctx, func(context.Context, AttemptInfo) (func(AttemptResult) error, error) {
		cancel()
		return func(r AttemptResult) error { outcome = r; return nil }, nil
	})
	c := fastClient(t, "http://127.0.0.1:1")
	if _, err := c.Post(ctx, "/model", nil); err == nil {
		t.Fatal("canceled request succeeded")
	}
	if outcome.State != AttemptNotSent {
		t.Fatalf("outcome=%+v", outcome)
	}
}

func TestPreflightRetriesCannotBypassAttemptGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "event: error\ndata: {}\n\n") }))
	defer srv.Close()
	var states []AttemptState
	calls := 0
	ctx := WithAttemptGate(context.Background(), func(context.Context, AttemptInfo) (func(AttemptResult) error, error) {
		calls++
		if calls > 1 {
			return nil, errors.New("no second reservation")
		}
		return func(r AttemptResult) error { states = append(states, r.State); return nil }, nil
	})
	_, err := fastClient(t, srv.URL).PostStream(ctx, "/model", nil, func(*http.Response) (StreamAccepted, *Error) {
		return StreamAccepted{}, StreamFailure("test", "overloaded", 503, true)
	})
	if err == nil || calls != 2 || len(states) != 1 || states[0] != AttemptUnknown {
		t.Fatalf("calls=%d states=%v err=%v", calls, states, err)
	}
}

func TestAccountingFailureStopsRetries(t *testing.T) {
	var sends atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { sends.Add(1); w.WriteHeader(503) }))
	defer srv.Close()
	accountingErr := errors.New("ledger write failed")
	ctx := WithAttemptGate(context.Background(), func(context.Context, AttemptInfo) (func(AttemptResult) error, error) {
		return func(AttemptResult) error { return accountingErr }, nil
	})
	_, err := fastClient(t, srv.URL).Post(ctx, "/model", nil)
	if !errors.Is(err, accountingErr) || sends.Load() != 1 {
		t.Fatalf("sends=%d err=%v", sends.Load(), err)
	}
}

func TestCredentialFailureReleasesOnlyUnsentAttempt(t *testing.T) {
	var outcomes []AttemptResult
	ctx := WithAttemptGate(context.Background(), func(context.Context, AttemptInfo) (func(AttemptResult) error, error) {
		return func(r AttemptResult) error { outcomes = append(outcomes, r); return nil }, nil
	})
	c := fastClient(t, "http://127.0.0.1:1")
	c.Header = func() (http.Header, error) { return nil, errors.New("credentials unavailable") }
	if _, err := c.Post(ctx, "/model", nil); err == nil {
		t.Fatal("missing credentials succeeded")
	}
	if len(outcomes) != 1 || outcomes[0].State != AttemptNotSent {
		t.Fatalf("outcomes=%v", outcomes)
	}
}

func TestCancellationAfterServerReceivesRequestRetainsUnknownCharge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-release }))
	defer srv.Close()
	var outcome AttemptResult
	ctx = WithAttemptGate(ctx, func(context.Context, AttemptInfo) (func(AttemptResult) error, error) {
		return func(r AttemptResult) error { outcome = r; return nil }, nil
	})
	_, err := fastClient(t, srv.URL).Post(ctx, "/model", nil)
	close(release)
	if !errors.Is(err, context.Canceled) || outcome.State != AttemptUnknown {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}
