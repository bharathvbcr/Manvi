package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fastClient(t *testing.T, url string) *Client {
	t.Helper()
	c := New("test", url, func() (http.Header, error) {
		h := http.Header{}
		h.Set("Authorization", "Bearer secret")
		return h, nil
	})
	// Keep the tests quick; the policy shape is what is under test, not the
	// wall-clock delay.
	c.Retry = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond, Jitter: 0}
	return c
}

func TestSuccessOnFirstAttempt(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("auth header = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	resp, err := fastClient(t, srv.URL).Post(context.Background(), "/v1/x", map[string]string{"a": "b"})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if calls != 1 {
		t.Fatalf("made %d calls, want 1", calls)
	}
}

func TestTransientFailuresRetryThenSucceed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	resp, err := fastClient(t, srv.URL).Post(context.Background(), "/v1/x", nil)
	if err != nil {
		t.Fatalf("should have recovered: %v", err)
	}
	defer resp.Body.Close()
	if calls != 3 {
		t.Fatalf("made %d calls, want 3", calls)
	}
}

// TestClientErrorsDoNotRetry is the rule that keeps a bad request from burning
// the whole budget and hiding the defect.
func TestClientErrorsDoNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("request-id", "req_abc123")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"temperature is not supported"}}`)
	}))
	defer srv.Close()

	_, err := fastClient(t, srv.URL).Post(context.Background(), "/v1/x", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("made %d calls; a 400 must not be retried", calls)
	}

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %T, want *transport.Error", err)
	}
	if e.Status != 400 || e.RequestID != "req_abc123" {
		t.Fatalf("error = %+v", e)
	}
	// The provider's own message is the fastest route to the cause; it must
	// survive rather than being collapsed into "request failed".
	if !strings.Contains(e.Body, "temperature is not supported") {
		t.Fatalf("body lost: %q", e.Body)
	}
	if e.Retryable() {
		t.Fatal("a 400 is not retryable")
	}
}

func TestExhaustedRetriesReportEveryAttempt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := fastClient(t, srv.URL).Post(context.Background(), "/v1/x", nil)
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error = %T", err)
	}
	if e.Attempts != 4 {
		t.Fatalf("attempts = %d, want 4", e.Attempts)
	}
	if !strings.Contains(e.Error(), "after 4 attempts") {
		t.Fatalf("message should state the attempt count: %s", e.Error())
	}
}

// TestRetryAfterIsObeyed: when the server says when it will be ready, guessing
// a shorter delay is how a rate limit becomes a ban.
func TestRetryAfterIsObeyed(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	start := time.Now()
	resp, err := c.Post(context.Background(), "/v1/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("waited %v; Retry-After: 1 must be honoured over the 1ms backoff", elapsed)
	}
}

// TestAbsurdRetryAfterIsBounded: obeying the server must not mean a buggy or
// hostile header can park the harness indefinitely.
func TestAbsurdRetryAfterIsBounded(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 8 * time.Second}
	if got := p.delayFor(1, 24*time.Hour, nil); got != DefaultMaxRetryAfter {
		t.Fatalf("delay = %v, want it clamped to %v", got, DefaultMaxRetryAfter)
	}

	// The ceiling is absolute, not derived from MaxDelay: a fast backoff must
	// not silently shorten how long we honour the server's own instruction.
	fast := RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond}
	if got := fast.delayFor(1, time.Second, nil); got != time.Second {
		t.Fatalf("delay = %v, want the full 1s Retry-After honoured despite a 5ms MaxDelay", got)
	}
}

func TestCancellationPropagatesAndStopsRetrying(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := fastClient(t, srv.URL)
	c.Retry = RetryPolicy{MaxAttempts: 10, BaseDelay: 50 * time.Millisecond, MaxDelay: time.Second}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	_, err := c.Post(ctx, "/v1/x", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if calls >= 10 {
		t.Fatalf("made %d calls; cancellation must stop the retry loop", calls)
	}
}

// TestCredentialFailureDoesNotRetry: retrying an unresolvable credential just
// delays the same error.
func TestCredentialFailureDoesNotRetry(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
	}))
	defer srv.Close()

	c := New("test", srv.URL, func() (http.Header, error) {
		return nil, errors.New("no API key configured")
	})
	c.Retry = RetryPolicy{MaxAttempts: 4, BaseDelay: time.Millisecond}

	_, err := c.Post(context.Background(), "/v1/x", nil)
	if err == nil || !strings.Contains(err.Error(), "no API key configured") {
		t.Fatalf("error = %v", err)
	}
	if calls != 0 {
		t.Fatalf("made %d requests; the credential failed before any was sent", calls)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	p := RetryPolicy{MaxAttempts: 8, BaseDelay: time.Second, MaxDelay: 4 * time.Second}
	got := []time.Duration{
		p.delayFor(1, 0, nil), p.delayFor(2, 0, nil),
		p.delayFor(3, 0, nil), p.delayFor(4, 0, nil),
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delay[%d] = %v, want %v (sequence %v)", i+1, got[i], want[i], got)
		}
	}
}

// --- SSE ---

func sseFrom(body string, done string) *SSE {
	return NewSSE(io.NopCloser(strings.NewReader(body)), done)
}

func TestSSEParsesNamedEvents(t *testing.T) {
	// Anthropic's shape: an event: name plus a data: payload.
	s := sseFrom("event: message_start\ndata: {\"a\":1}\n\nevent: content_block_delta\ndata: {\"b\":2}\n\n", "")
	defer s.Close()

	first, err := s.Next()
	if err != nil || first.Name != "message_start" || string(first.Data) != `{"a":1}` {
		t.Fatalf("first = %+v, err = %v", first, err)
	}
	second, err := s.Next()
	if err != nil || second.Name != "content_block_delta" {
		t.Fatalf("second = %+v, err = %v", second, err)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

// TestSSEHonoursTheDoneSentinel covers the OpenAI-compatible shape xAI uses:
// data-only events terminated by a literal [DONE].
func TestSSEHonoursTheDoneSentinel(t *testing.T) {
	s := sseFrom("data: {\"a\":1}\n\ndata: [DONE]\n\ndata: {\"never\":true}\n\n", "[DONE]")
	defer s.Close()

	if _, err := s.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("[DONE] must terminate the stream, got %v", err)
	}
	// And it stays terminated — content after the sentinel is never delivered.
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("stream must stay closed, got %v", err)
	}
}

func TestSSEIgnoresKeepAlivesAndComments(t *testing.T) {
	s := sseFrom(": ping\n\n\ndata: {\"a\":1}\n\n", "")
	defer s.Close()
	event, err := s.Next()
	if err != nil || string(event.Data) != `{"a":1}` {
		t.Fatalf("event = %+v, err = %v", event, err)
	}
}

func TestSSEJoinsMultiLineData(t *testing.T) {
	s := sseFrom("data: line one\ndata: line two\n\n", "")
	defer s.Close()
	event, err := s.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Data) != "line one\nline two" {
		t.Fatalf("data = %q", event.Data)
	}
}

// TestSSEDeliversAFinalEventWithoutTrailingBlankLine: a stream cut off after
// its last payload must still yield that payload rather than dropping it.
func TestSSEDeliversAFinalEventWithoutTrailingBlankLine(t *testing.T) {
	s := sseFrom("data: {\"a\":1}", "")
	defer s.Close()
	event, err := s.Next()
	if err != nil || string(event.Data) != `{"a":1}` {
		t.Fatalf("event = %+v, err = %v", event, err)
	}
	if _, err := s.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestSSEOverAnHTTPResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range []string{"data: {\"i\":0}\n\n", "data: {\"i\":1}\n\n", "data: [DONE]\n\n"} {
			_, _ = io.WriteString(w, chunk)
			flusher.Flush()
		}
	}))
	defer srv.Close()

	resp, err := fastClient(t, srv.URL).Post(context.Background(), "/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	stream := NewSSE(resp.Body, "[DONE]")
	defer stream.Close()

	count := 0
	for {
		_, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("read %d events, want 2", count)
	}
}

func TestIsLoopbackURL(t *testing.T) {
	cases := []struct {
		url      string
		loopback bool
	}{
		{"http://127.0.0.1:8000/v1", true},
		{"http://localhost:11434", true},
		{"http://[::1]:8080", true},
		{"http://0.0.0.0:8000/v1", true},
		{"https://api.anthropic.com", false},
		{"https://api.x.ai/v1", false},
		{"https://generativelanguage.googleapis.com", false},
	}
	for _, tc := range cases {
		got := isLoopbackURL(tc.url)
		if got != tc.loopback {
			t.Errorf("isLoopbackURL(%q) = %v, want %v", tc.url, got, tc.loopback)
		}
	}
}

func TestLocalClientUsesLoopbackTransport(t *testing.T) {
	c := New("local", "http://127.0.0.1:8000/v1", func() (http.Header, error) {
		return http.Header{}, nil
	})
	tr, ok := c.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", c.HTTP.Transport)
	}
	if tr.MaxIdleConnsPerHost != 64 {
		t.Errorf("expected MaxIdleConnsPerHost=64, got %d", tr.MaxIdleConnsPerHost)
	}
	if tr.MaxIdleConns != 256 {
		t.Errorf("expected MaxIdleConns=256, got %d", tr.MaxIdleConns)
	}
	if !tr.DisableCompression {
		t.Error("expected DisableCompression=true for loopback")
	}
}
