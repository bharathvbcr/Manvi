package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- 1. the shared jitter source ---

// TestConcurrentRetriesDoNotRaceOnTheJitterSource. Jitter is on by default
// (DefaultRetryPolicy ships 0.2), so every turn that hits a 429 or a 503 draws
// a random number, and a harness fans several provider calls out at once. A
// *rand.Rand shared across those goroutines is not safe for concurrent use:
// the draw corrupts its internal state, and the race detector reports it. The
// existing retry tests miss this because they pin Jitter to 0.
func TestConcurrentRetriesDoNotRaceOnTheJitterSource(t *testing.T) {
	if DefaultRetryPolicy().Jitter <= 0 {
		t.Fatal("this test only means something while jitter is on by default")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New("test", srv.URL, func() (http.Header, error) { return http.Header{}, nil })
	// The default policy's shape, with the wall-clock delays shrunk: what is
	// under test is the shared draw, not how long it waits.
	c.Retry = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 4 * time.Millisecond, Jitter: 0.2}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Post(context.Background(), "/v1/x", nil)
		}()
	}
	wg.Wait()
}

// --- 2. keep-alive comments must not re-arm the watchdog ---

// TestKeepAliveCommentsDoNotCountAsProgress. A proxy or a server that emits
// only ": ping" heartbeats produces no tokens at all while re-arming the
// watchdog on every beat, which defeats the entire purpose of the watchdog: the
// stream is silent in the only sense that matters and the harness waits out the
// whole turn budget anyway.
func TestKeepAliveCommentsDoNotCountAsProgress(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		// One real frame, so the stream is past time-to-first-token and the
		// inter-token limit is what governs from here.
		_, _ = pw.Write([]byte("data: {\"a\":1}\n\n"))
		for i := 0; i < 40; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := pw.Write([]byte(": ping\n")); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	sse := NewSSEWithStall(pr, "[DONE]", 200*time.Millisecond)
	defer func() { _ = sse.Close() }()

	if _, err := sse.Next(); err != nil {
		t.Fatalf("the first frame should arrive normally: %v", err)
	}

	start := time.Now()
	_, err := sse.Next()
	elapsed := time.Since(start)

	var stalled *ErrStalled
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v (%T) after %s of pure heartbeats, want ErrStalled — a "+
			"comment is not output", err, err, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the watchdog took %s to fire on a 200ms limit", elapsed)
	}
}

// --- 3. progress within one long line ---

// TestALongLineArrivingContinuouslyIsNotAStall. A local model emitting a large
// tool-call payload sends one enormous data: line. Resetting the watchdog only
// when a line *completes* abandons that stream even though bytes are arriving
// the whole time — the harness kills the request precisely when the model is
// doing the most work.
func TestALongLineArrivingContinuouslyIsNotAStall(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write([]byte(`data: {"args":"`)); err != nil {
			return
		}
		for i := 0; i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			if _, err := pw.Write([]byte(strings.Repeat("x", 512))); err != nil {
				return
			}
		}
		_, _ = pw.Write([]byte("\"}\n\ndata: [DONE]\n\n"))
		_ = pw.Close()
	}()

	// Every gap is 50ms, well inside the limit; the line takes a second to
	// arrive, well outside it.
	sse := NewSSEWithStall(pr, "[DONE]", 200*time.Millisecond)
	defer func() { _ = sse.Close() }()

	event, err := sse.Next()
	if err != nil {
		t.Fatalf("a continuously arriving line was abandoned: %v", err)
	}
	if want := 20 * 512; len(event.Data) < want {
		t.Fatalf("data = %d bytes, want at least %d", len(event.Data), want)
	}
	if _, err := sse.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

// TestAnEndlessLineIsBoundedRatherThanBuffered. bufio grows one line without
// limit, so a server (or a corrupted connection) that never sends a newline
// takes the harness's whole address space with it. The bound must be far above
// any legitimate frame and must be reported, not silently truncated into a
// payload the decoder will misread.
func TestAnEndlessLineIsBoundedRatherThanBuffered(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		chunk := []byte("data: " + strings.Repeat("x", 1<<20))
		for {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
	}()

	sse := NewSSEWithStall(pr, "[DONE]", 0)
	defer func() { _ = sse.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := sse.Next()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want a refusal to keep buffering", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("a line with no newline was buffered without bound")
	}
}

// --- 4. a timer that fires while it is being re-armed ---

type countingCloser struct {
	mu sync.Mutex
	n  int
}

func (c *countingCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *countingCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (c *countingCloser) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestTheWatchdogClosesTheBodyAtMostOnce. reset() takes the mutex before the
// fired timer's goroutine does, sees tripped == false, and re-arms an AfterFunc
// timer that is already mid-fire — so trip() runs again and the body is closed
// twice or more. http.Response.Body tolerates that; an arbitrary io.Closer,
// which is what the exported constructor accepts, does not.
func TestTheWatchdogClosesTheBodyAtMostOnce(t *testing.T) {
	for i := 0; i < 400; i++ {
		c := &countingCloser{}
		// Both limits equal, so the phase the clock is in cannot be what makes
		// this pass or fail.
		w := newStallWatchdog(c, time.Millisecond, time.Millisecond)

		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				deadline := time.Now().Add(4 * time.Millisecond)
				for time.Now().Before(deadline) {
					w.progress()
				}
			}()
		}
		wg.Wait()
		time.Sleep(5 * time.Millisecond)
		w.stop()

		if n := c.count(); n > 1 {
			t.Fatalf("iteration %d: the body was closed %d times; progress that races "+
				"the firing must not re-arm a timer that already fired", i, n)
		}
	}
}

// --- 5. concurrent Close ---

type closeCounter struct {
	n int32
}

func (c *closeCounter) Read([]byte) (int, error) { return 0, io.EOF }

func (c *closeCounter) Close() error {
	atomic.AddInt32(&c.n, 1)
	return nil
}

// TestConcurrentCloseIsSafeAndReleasesTheBodyOnce. Close is exported, and the
// obvious use — a reader goroutine and the turn's cancellation path both
// tearing the stream down — has two goroutines writing s.closer at once.
func TestConcurrentCloseIsSafeAndReleasesTheBodyOnce(t *testing.T) {
	body := &closeCounter{}
	sse := NewSSE(body, "[DONE]")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sse.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wg.Wait()

	if n := atomic.LoadInt32(&body.n); n != 1 {
		t.Fatalf("the body was closed %d times, want exactly 1", n)
	}
}

// --- 7. cancellation must not be classified as retryable ---

// TestACancelledRequestIsNotRetryable. Error.Retryable() answers true for any
// Status == 0, which includes a context cancellation, so a cancelled call is
// classified as a transient fault and only the loop's own ctx.Done() arm keeps
// it from being retried. That makes the taxonomy lie to every other caller of
// Retryable().
func TestACancelledRequestIsNotRetryable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// Bounded so a failing assertion cannot wedge the test server's own
		// shutdown, which waits for outstanding handlers.
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := c.Post(ctx, "/v1/x", nil)
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %v (%T), want *transport.Error", err, err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to unwrap to context.Canceled", err)
	}
	if e.Retryable() {
		t.Fatal("a cancelled request was classified as retryable; cancellation is the " +
			"caller's decision and no retry of it can succeed")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("made %d requests, want 1", n)
	}
}

// --- time to first token vs the gap between tokens ---

// TestTimeToFirstTokenIsNotBoundedByTheInterTokenLimit. Confirmed in
// production: a local MLX server doing a cold prefill of a ~40k-token prompt
// emitted nothing for over five minutes and the harness killed a healthy turn.
// Prefill latency scales with prompt length and is legitimately minutes; the
// gap between tokens is what indicates a wedged stream. One budget cannot bound
// both.
func TestTimeToFirstTokenIsNotBoundedByTheInterTokenLimit(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		// Silence far longer than the inter-token limit, then a healthy stream.
		time.Sleep(700 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"a\":1}\n\ndata: [DONE]\n\n"))
		_ = pw.Close()
	}()

	sse := NewSSEWithStall(pr, "[DONE]", 100*time.Millisecond)
	defer func() { _ = sse.Close() }()

	event, err := sse.Next()
	if err != nil {
		t.Fatalf("a prefill that outlasted the inter-token limit was reported as a "+
			"stall: %v", err)
	}
	if string(event.Data) != `{"a":1}` {
		t.Fatalf("data = %q", event.Data)
	}
}

// TestTheStallMessageDoesNotAssertWhatTheTransportCannotKnow. The message
// declared "This is not a slow model — it is a stopped one", which the
// transport has no way to determine: it sees silence, and a cold prefill and a
// wedged process look identical. Sending an operator to restart a healthy
// server is worse than saying nothing.
func TestTheStallMessageDoesNotAssertWhatTheTransportCannotKnow(t *testing.T) {
	body := newBlockingBody("data: {\"a\":1}\n\n")
	sse := NewSSEWithStall(body, "[DONE]", 100*time.Millisecond)
	defer func() { _ = sse.Close() }()

	if _, err := sse.Next(); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	_, err := sse.Next()
	var stalled *ErrStalled
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}

	msg := err.Error()
	for _, claim := range []string{"it is a stopped one", "the server had stopped sending"} {
		if strings.Contains(msg, claim) {
			t.Errorf("the message asserts %q as fact, which this layer cannot "+
				"distinguish from a slow server: %v", claim, err)
		}
	}
	if !strings.Contains(msg, "no output") {
		t.Errorf("the message does not name the silence: %v", err)
	}
}

// --- the read error that names the cause must be the one that survives ---

// twoErrorBody returns first on its first read and second on every read after,
// which is how a cancelled http.Response.Body behaves: net/http maps the first
// failed read to the context error that explains it, and a second read of the
// same dead connection reports only that the socket is closed.
type twoErrorBody struct {
	first  error
	second error
	reads  int
}

func (b *twoErrorBody) Read([]byte) (int, error) {
	b.reads++
	if b.reads == 1 {
		return 0, b.first
	}
	return 0, b.second
}

func (b *twoErrorBody) Close() error { return nil }

// TestTheFirstReadErrorIsTheOneReported. The stream classifies each line before
// consuming it, and bufio.Peek *takes* the pending read error out of the buffer
// when it reports it. Dropping it there and reading again does not rediscover
// it: the second read of a cancelled connection answers "use of closed network
// connection", so the caller is told the socket broke instead of that the turn
// was cancelled — and a caller that distinguishes a cancelled turn from a
// transport fault then retries something the user stopped.
func TestTheFirstReadErrorIsTheOneReported(t *testing.T) {
	body := &twoErrorBody{
		first:  context.Canceled,
		second: errors.New("use of closed network connection"),
	}
	// The watchdog must be armed: it is what puts the classifying peek in front
	// of the read.
	sse := NewSSEWithStall(body, "[DONE]", time.Minute)
	defer func() { _ = sse.Close() }()

	_, err := sse.Next()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled — the error that names the cause "+
			"must not be swallowed by the line classification", err)
	}
	if body.reads > 1 {
		t.Errorf("the body was read %d times after it had already failed", body.reads)
	}
}
