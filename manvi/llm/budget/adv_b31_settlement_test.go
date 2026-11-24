package budget_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm"
	"github.com/bharathvbcr/Manvi/manvi/llm/adaptertest"
	"github.com/bharathvbcr/Manvi/manvi/llm/anthropic"
	"github.com/bharathvbcr/Manvi/manvi/llm/budget"
)

// advWrap lets a test replace what the metered provider's inner Stream returns
// while the real adapter still drives the admission gate.
type advWrap struct {
	*anthropic.Adapter
	stream func(ctx context.Context, req llm.Request, inner *anthropic.Adapter) (llm.Stream, error)
}

func (w *advWrap) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	return w.stream(ctx, req, w.Adapter)
}

func advAssertSettled(t *testing.T, s budget.Snapshot, calls int32) {
	t.Helper()
	if len(s.InFlight) != 0 {
		t.Errorf("attempts leaked in flight: %+v", s.InFlight)
	}
	if got := s.Settled + s.RetainedAttempts + s.RefusedAttempts; got != int64(calls) {
		t.Errorf("settled %d + retained %d + refused %d = %d resolutions for %d admitted attempts: %+v",
			s.Settled, s.RetainedAttempts, s.RefusedAttempts, got, calls, s)
	}
}

func TestAdvB31OpsStreamErrorAfterAdmittingSeveralAttemptsRetainsEach(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	if _, err := p.Stream(context.Background(), request()); err == nil {
		t.Fatal("a server answering 503 forever produced a stream")
	}
	if calls.Load() < 2 {
		t.Skipf("only %d attempt was made; the scenario needs a retry", calls.Load())
	}
	s := snapshot(t, l)
	advAssertSettled(t, s, calls.Load())
	if s.SpentNanoUSD != 0 || s.RetainedNanoUSD <= 0 {
		t.Errorf("spent %d retained %d: unanswered attempts must be retained, never spent or released", s.SpentNanoUSD, s.RetainedNanoUSD)
	}
}

func TestAdvB31OpsCancelledDuringSecondAttemptKeepsBothReservations(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		cancel()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	_, err := p.Stream(ctx, request())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	s := snapshot(t, l)
	advAssertSettled(t, s, calls.Load())
	if calls.Load() != 2 || s.RetainedAttempts != 2 || s.SpentNanoUSD != 0 {
		t.Errorf("calls %d snapshot %+v, want both attempts retained and nothing spent", calls.Load(), s)
	}
}

// A provider that sends a request and then returns neither a stream nor an
// error: the admitted attempt was sent and must be kept.
func TestAdvB31OpsNilStreamNilErrorRetainsTheSentAttempt(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	inner := &advWrap{Adapter: anthropic.New(server.URL, adaptertest.Secret("fixture-key"))}
	inner.stream = func(ctx context.Context, req llm.Request, a *anthropic.Adapter) (llm.Stream, error) {
		if _, err := a.Stream(ctx, req); err != nil {
			return nil, err
		}
		return nil, nil
	}
	p := &budget.Provider{Inner: inner, Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err == nil || stream != nil {
		t.Fatalf("(%v, %v): a nil stream with a nil error was passed on", stream, err)
	}
	s := snapshot(t, l)
	advAssertSettled(t, s, calls.Load())
	if s.RetainedAttempts != 1 || s.RetainedNanoUSD <= 0 || s.SpentNanoUSD != 0 {
		t.Errorf("snapshot %+v, want the one sent attempt retained", s)
	}
}

// A provider that claims the gate but returns a stream that never went through
// it: nothing can settle. The answer must not be a silent success.
func TestAdvB31OpsStreamThatNeverPassedTheGateIsNotASilentSuccess(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	inner := &advWrap{Adapter: anthropic.New("http://127.0.0.1:0", adaptertest.Secret("fixture-key"))}
	inner.stream = func(ctx context.Context, req llm.Request, a *anthropic.Adapter) (llm.Stream, error) {
		return advCanned{}, nil
	}
	p := &budget.Provider{Inner: inner, Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		return // refusing up front is also fail-closed
	}
	_, rerr := stream.Response()
	cerr := stream.Close()
	s := snapshot(t, l)
	if rerr == nil && cerr == nil && s.Settled == 0 && s.SpentNanoUSD == 0 {
		t.Fatalf("an unmetered stream completed with no error and no ledger entry: %+v", s)
	}
}

type advCanned struct{}

func (advCanned) Next() (llm.Chunk, error) { return llm.Chunk{}, io.EOF }
func (advCanned) Close() error             { return nil }
func (advCanned) Response() (llm.Response, error) {
	return llm.Response{Usage: llm.Usage{InputTokens: 1000, OutputTokens: 10}}, nil
}

func TestAdvB31OpsResponseThenCloseThenResponseSettlesOnce(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	drain(t, stream)
	for range 3 {
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Response(); err != nil {
			t.Fatal(err)
		}
	}
	s := snapshot(t, l)
	if s.Settled != 1 || s.SpentNanoUSD != turnCost || s.RetainedAttempts != 0 || len(s.InFlight) != 0 {
		t.Fatalf("repeated Close/Response changed the books: %+v, want one settlement of %d", s, turnCost)
	}
}

func TestAdvB31OpsCloseBeforeResponseRetainsAndNeverSettles(t *testing.T) {
	l := ledger(t, 1_000_000_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	stream, err := p.Stream(context.Background(), request())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	_, _ = stream.Response()
	s := snapshot(t, l)
	if s.Settled != 0 || s.RetainedAttempts != 1 || len(s.InFlight) != 0 || s.SpentNanoUSD != 0 {
		t.Fatalf("%+v, want one retained attempt", s)
	}
}

// Many streams share one ledger: totals must equal the sum of what each did.
func TestAdvB31OpsConcurrentStreamsKeepTheBooksExact(t *testing.T) {
	l := ledger(t, 1_000_000_000_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, turnSSE)
	}))
	defer server.Close()
	p := &budget.Provider{Inner: anthropic.New(server.URL, adaptertest.Secret("fixture-key")), Ledger: l}
	const n = 24
	done := make(chan error, n)
	for range n {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s, err := p.Stream(ctx, request())
			if err != nil {
				done <- err
				return
			}
			for {
				if _, err := s.Next(); err != nil {
					break
				}
			}
			_, err = s.Response()
			done <- errors.Join(err, s.Close())
		}()
	}
	for range n {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	s := snapshot(t, l)
	if s.Settled != n || s.SpentNanoUSD != n*turnCost || len(s.InFlight) != 0 || s.RetainedAttempts != 0 {
		t.Fatalf("%+v, want %d settlements of %d", s, n, turnCost)
	}
}
