package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"manvi/llm"
	"manvi/llm/transport"
)

// The stall watchdog is the harness's only answer to a real failure: the server
// accepted the request, emitted a token, and then went silent for ten minutes
// while the turn budget drained. The transport implements the watchdog; these
// tests are about whether this adapter actually arms it, whether it fires only
// on silence, and whether what the operator finally reads names the fault.

// wedgeServer answers with an SSE stream that emits the given frames and then
// goes quiet without ending the stream — the wedged-server shape. It reports
// through bodyClosed when the client tore the connection down, which is how the
// test observes that the response body was actually released.
func wedgeServer(t *testing.T, frames ...string) (*httptest.Server, <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, f := range frames {
			_, _ = fmt.Fprint(w, f+"\n\n")
		}
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			close(closed)
		case <-time.After(20 * time.Second):
			// Bounded so a failing assertion cannot wedge the test binary
			// itself; 20s is far beyond any stall limit used here.
			t.Error("the client never closed the stalled body")
			close(closed)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, closed
}

func stallAdapter(t *testing.T, baseURL string, stall time.Duration) *Adapter {
	t.Helper()
	return New(Options{
		Name:         "local",
		BaseURL:      baseURL,
		StallTimeout: stall,
		Validate:     func(llm.Request) error { return nil },
		Header:       func() (http.Header, error) { return http.Header{}, nil },
	})
}

// TestAWedgedStreamIsAbandonedAndNamedAsAStall is the whole point of the
// setting. Without it the read blocks until the turn deadline, and the operator
// is told only that the turn ran out of time.
func TestAWedgedStreamIsAbandonedAndNamedAsAStall(t *testing.T) {
	srv, closed := wedgeServer(t, `data: {"choices":[{"delta":{"content":"th"}}]}`)
	a := stallAdapter(t, srv.URL, 200*time.Millisecond)

	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, err := s.Next(); err != nil {
		t.Fatalf("the first frame should arrive normally: %v", err)
	}

	start := time.Now()
	_, err = s.Next()
	elapsed := time.Since(start)

	var stalled *transport.ErrStalled
	if !errors.As(err, &stalled) {
		t.Fatalf("Next err = %v (%T), want transport.ErrStalled — a wedged server must not "+
			"surface as a generic transport or decode failure", err, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the watchdog took %s to fire on a 200ms limit", elapsed)
	}
	// The two ways this error is misread: as a transport fault (restart the
	// network) or as bad data (a decode bug). It must read as neither.
	msg := err.Error()
	for _, wrong := range []string{"undecodable", "unexpected EOF", "use of closed network connection"} {
		if strings.Contains(msg, wrong) {
			t.Errorf("the stall was reported as %q: %v", wrong, err)
		}
	}
	if !strings.Contains(msg, "no output") {
		t.Errorf("the message does not name the silence: %v", err)
	}
	var timeouter interface{ Timeout() bool }
	if !errors.As(err, &timeouter) || !timeouter.Timeout() {
		t.Errorf("a stall must classify as a timeout for callers that branch on it: %v", err)
	}

	// The body must be released, not merely abandoned: a local server holds the
	// slot for an open connection, and the next turn queues behind it.
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the response body was never closed after the stall")
	}
}

// TestASettledStallIsReportedByResponseToo. A caller that reads Next to its end
// and then asks for the settled response must be told the server went silent —
// not that it broke the stream protocol by asking early. A check that could not
// run must not report as one that ran.
func TestASettledStallIsReportedByResponseToo(t *testing.T) {
	srv, _ := wedgeServer(t, `data: {"choices":[{"delta":{"content":"th"}}]}`)
	a := stallAdapter(t, srv.URL, 150*time.Millisecond)

	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	for {
		if _, err := s.Next(); err != nil {
			break
		}
	}

	_, err = s.Response()
	var stalled *transport.ErrStalled
	if !errors.As(err, &stalled) {
		t.Fatalf("Response err = %v, want the stall; a wedged server must not be reported "+
			"as a caller protocol error", err)
	}
}

// TestASlowButLiveStreamOutlivesTheStallTimeout. The failure this guards is the
// opposite one: a watchdog that kills a healthy generation is worse than none,
// because it makes a slow model unusable and looks like a server fault.
func TestASlowButLiveStreamOutlivesTheStallTimeout(t *testing.T) {
	const gap = 40 * time.Millisecond
	const frames = 8
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < frames; i++ {
			time.Sleep(gap)
			_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"tok"}}]}`+"\n\n")
			w.(http.Flusher).Flush()
		}
		_, _ = fmt.Fprint(w, `data: {"choices":[{"finish_reason":"stop"}]}`+"\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	// Total stream duration is well past the limit; every individual gap is
	// under it. Only a bound on the gap survives this.
	a := stallAdapter(t, srv.URL, 150*time.Millisecond)
	// Timed from before the request: the response headers do not arrive until
	// the first frame does, so starting the clock afterwards would exclude a
	// gap and understate the stream's real duration.
	start := time.Now()
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()

	seen := 0
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("a slow but live stream was abandoned after %d chunks: %v", seen, err)
		}
		if c.Kind == llm.ChunkText {
			seen++
		}
	}
	if elapsed := time.Since(start); elapsed < gap*frames {
		t.Fatalf("the stream finished in %s, faster than the server could have sent it", elapsed)
	}
	if seen != frames {
		t.Fatalf("text chunks = %d, want %d", seen, frames)
	}
	resp, err := s.Response()
	if err != nil {
		t.Fatalf("Response: %v", err)
	}
	if resp.Message.Text() != strings.Repeat("tok", frames) {
		t.Fatalf("text = %q; a healthy stream lost content", resp.Message.Text())
	}
}

// TestCallerCancellationIsNotMaskedByTheStallTimer. Cancellation must win and
// must say so: reporting a cancelled turn as a server stall sends an operator
// to restart a server that was working.
func TestCallerCancellationIsNotMaskedByTheStallTimer(t *testing.T) {
	srv, closed := wedgeServer(t, `data: {"choices":[{"delta":{"content":"th"}}]}`)
	// Long enough that the watchdog cannot be what ends this stream.
	a := stallAdapter(t, srv.URL, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	s, err := a.Stream(ctx, llm.Request{Model: "qwen"})
	if err != nil {
		cancel()
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err != nil {
		cancel()
		t.Fatalf("the first frame should arrive normally: %v", err)
	}

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err = s.Next()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next err = %v, want context.Canceled", err)
	}
	var stalled *transport.ErrStalled
	if errors.As(err, &stalled) {
		t.Fatalf("a cancelled turn was reported as a server stall: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not tear down the request")
	}
}

// TestAnUnsetStallTimeoutMeansTheDocumentedDefault. Zero must not read as "no
// wait at all", which would abandon every stream at the first pause, and must
// not read as "no watchdog", which is what the setting exists to prevent.
func TestAnUnsetStallTimeoutMeansTheDocumentedDefault(t *testing.T) {
	a := New(Options{
		Name:     "local",
		BaseURL:  "http://127.0.0.1:1",
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	if a.opts.StallTimeout != DefaultStallTimeout {
		t.Fatalf("StallTimeout = %v, want the documented default %v", a.opts.StallTimeout, DefaultStallTimeout)
	}
	if DefaultStallTimeout <= 0 {
		t.Fatal("a non-positive default would disable the watchdog everywhere")
	}

	// And behaviourally: a pause far longer than an instant, far shorter than
	// the default, is not a stall.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"a"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(250 * time.Millisecond)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	def := New(Options{
		Name:     "local",
		BaseURL:  srv.URL,
		Validate: func(llm.Request) error { return nil },
		Header:   func() (http.Header, error) { return http.Header{}, nil },
	})
	s, err := def.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	for {
		if _, err := s.Next(); err != nil {
			if !errors.Is(err, io.EOF) {
				t.Fatalf("an unset stall timeout abandoned a healthy stream: %v", err)
			}
			break
		}
	}
}

// TestANegativeStallTimeoutDisablesTheWatchdog documents the escape hatch, and
// proves it is the *only* way to get an unwatched stream.
func TestANegativeStallTimeoutDisablesTheWatchdog(t *testing.T) {
	srv, _ := wedgeServer(t, `data: {"choices":[{"delta":{"content":"th"}}]}`)
	a := stallAdapter(t, srv.URL, -1)
	if a.opts.StallTimeout != -1 {
		t.Fatalf("a negative StallTimeout was rewritten to %v", a.opts.StallTimeout)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s, err := a.Stream(ctx, llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.Next(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	_, err = s.Next()
	var stalled *transport.ErrStalled
	if errors.As(err, &stalled) {
		t.Fatalf("the watchdog fired despite being disabled: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; with the watchdog off only the caller's deadline should end this", err)
	}
}

// TestAStalledStreamLeaksNoGoroutines. The watchdog closes the body from a
// timer; if that left the reader parked or the connection held, a turn that
// survived one stall would accumulate a wedged goroutine per attempt.
func TestAStalledStreamLeaksNoGoroutines(t *testing.T) {
	before := stableGoroutines()

	for i := 0; i < 5; i++ {
		func() {
			srv, closed := wedgeServer(t, `data: {"choices":[{"delta":{"content":"th"}}]}`)
			// Closed here rather than at cleanup: a server left listening keeps
			// its own goroutines, which would be counted as the leak.
			defer srv.Close()
			a := stallAdapter(t, srv.URL, 100*time.Millisecond)
			s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			for {
				if _, err := s.Next(); err != nil {
					break
				}
			}
			if err := s.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
				t.Fatalf("Close after a stall: %v", err)
			}
			select {
			case <-closed:
			case <-time.After(5 * time.Second):
				t.Fatal("the stalled body was never closed")
			}
		}()
	}

	after := stableGoroutines()
	// A small allowance for runtime and idle-connection bookkeeping; five
	// leaked readers would be five over.
	if after > before+2 {
		t.Fatalf("goroutines: %d before, %d after five stalls — the readers were not released",
			before, after)
	}
}

// stableGoroutines waits for the count to settle, so a test does not measure
// goroutines that were merely on their way out.
func stableGoroutines() int {
	last := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		time.Sleep(20 * time.Millisecond)
		runtime.GC()
		now := runtime.NumGoroutine()
		if now == last {
			return now
		}
		last = now
	}
	return last
}

// TestCloseIsSafeAfterAStall. The loop closes the stream in a defer; a
// watchdog that already closed the body must not turn that into a second
// failure that masks the first.
func TestCloseIsSafeAfterAStall(t *testing.T) {
	srv, _ := wedgeServer(t, `data: {"choices":[{"delta":{"content":"th"}}]}`)
	a := stallAdapter(t, srv.URL, 100*time.Millisecond)
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for {
		if _, err := s.Next(); err != nil {
			break
		}
	}
	for i := 0; i < 3; i++ {
		if err := s.Close(); err != nil {
			t.Fatalf("Close #%d after a stall: %v", i+1, err)
		}
	}
}

// TestTheStallLimitIsNotSentToTheServer. It is a harness-side bound; a server
// that rejects unknown fields must not see it.
func TestTheStallLimitIsNotSentToTheServer(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	a := stallAdapter(t, srv.URL, 3*time.Second)
	s, err := a.Stream(context.Background(), llm.Request{Model: "qwen"})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = s.Close()
	for _, key := range []string{"stall_timeout", "stall", "timeout"} {
		if _, present := body[key]; present {
			t.Errorf("%s was sent to the server; it is a harness-side bound", key)
		}
	}
}
