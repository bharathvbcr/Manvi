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

func readAll(t *testing.T, s *SSE) (events int, err error) {
	t.Helper()
	for {
		if _, err = s.Next(); err != nil {
			return events, err
		}
		events++
	}
}

// TestDoneSentinelWithoutATrailingBlankLine: `data: [DONE]` followed by EOF
// was returned as an ordinary event, and every decoder then failed to parse it.
func TestDoneSentinelWithoutATrailingBlankLine(t *testing.T) {
	for name, tail := range map[string]string{
		"newline":              "data: [DONE]\n",
		"no newline":           "data: [DONE]",
		"crlf":                 "data: [DONE]\r\n",
		"blank line (control)": "data: [DONE]\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSSE(io.NopCloser(strings.NewReader("data: {\"a\":1}\n\n"+tail)), "[DONE]")
			events, err := readAll(t, s)
			if err != io.EOF || events != 1 {
				t.Fatalf("events=%d err=%v, want the one real event then a clean EOF", events, err)
			}
			if !s.Terminated() {
				t.Fatal("the sentinel arrived but Terminated() is false")
			}
		})
	}
}

// TestABareEOFIsNotTerminated: the two endings are indistinguishable through
// Next alone; Terminated is what tells a finished stream from a cut one.
func TestABareEOFIsNotTerminated(t *testing.T) {
	for name, body := range map[string]string{
		"after a blank line":   "data: {\"a\":1}\n\n",
		"mid event":            "data: {\"a\":1}\n",
		"empty body":           "",
		"sentinel not matched": "data: [DONE] extra\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			s := NewSSE(io.NopCloser(strings.NewReader(body)), "[DONE]")
			_, _ = readAll(t, s)
			if s.Terminated() {
				t.Fatal("a stream that was cut off reports Terminated()")
			}
		})
	}
	if NewSSE(io.NopCloser(strings.NewReader("data: x\n\n")), "").Terminated() {
		t.Fatal("a stream with no sentinel cannot be Terminated")
	}
}

// TestTruncatedStreamIsRetryableAndInStream pins the one shared failure type.
func TestTruncatedStreamIsRetryableAndInStream(t *testing.T) {
	e := TruncatedStream("p", "message_stop")
	if !e.Retryable() || !e.InStream() || !strings.Contains(e.Error(), "message_stop") {
		t.Fatalf("%#v / %v", e, e)
	}
	if strings.Count(e.Error(), "the stream ended before") != 1 {
		t.Fatalf("message repeated: %q", e.Error())
	}
}

func muteServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { close(release); s.Close() })
	return s, &hits
}

// TestAMuteServerIsBoundedByTheResponseHeaderTimeout: nothing but the caller's
// context used to bound the wait for headers, and the TUI's has no deadline.
func TestAMuteServerIsBoundedByTheResponseHeaderTimeout(t *testing.T) {
	s, hits := muteServer(t)
	c := New("p", s.URL, nil)
	c.ResponseHeaderTimeout = 40 * time.Millisecond
	c.Retry = RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}

	start := time.Now()
	_, err := c.Post(context.Background(), "/x", map[string]string{"a": "b"})
	if err == nil {
		t.Fatal("a mute server answered")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s", d)
	}
	var timeout *ErrHeaderTimeout
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("err = %v, want a typed header timeout", err)
	}
	var te *Error
	if !errors.As(err, &te) || !te.Retryable() || te.Attempts != 3 {
		t.Fatalf("err = %#v, want a retryable failure after 3 attempts", err)
	}
	if hits.Load() != 3 {
		t.Fatalf("server saw %d attempts, want 3", hits.Load())
	}
}

// TestTheHeaderTimeoutDoesNotCutOffAHealthyStream: it is a time-to-first-byte
// bound, so a body that keeps flowing for many times the limit is read whole.
func TestTheHeaderTimeoutDoesNotCutOffAHealthyStream(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for i := 0; i < 8; i++ {
			_, _ = io.WriteString(w, "data: x\n\n")
			f.Flush()
			time.Sleep(25 * time.Millisecond)
		}
	}))
	defer s.Close()
	c := New("p", s.URL, nil)
	c.ResponseHeaderTimeout = 60 * time.Millisecond
	resp, err := c.Post(context.Background(), "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || strings.Count(string(body), "data: x") != 8 {
		t.Fatalf("body=%q err=%v; a stream five times the header timeout long was cut off", body, err)
	}
}

// TestAHeaderTimeoutIsRetriedAndTheNextAttemptSucceeds: retry is per attempt.
func TestAHeaderTimeoutIsRetriedAndTheNextAttemptSucceeds(t *testing.T) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body) // the server only notices a dropped connection once the body is read
		if hits.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer s.Close()
	c := New("p", s.URL, nil)
	c.ResponseHeaderTimeout = 40 * time.Millisecond
	c.Retry = RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond}
	resp, err := c.Post(context.Background(), "/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if b, _ := io.ReadAll(resp.Body); string(b) != "ok" || hits.Load() != 2 {
		t.Fatalf("body=%q hits=%d", b, hits.Load())
	}
}

// TestTheCallersCancellationIsNotAHeaderTimeout keeps the two apart: a
// cancelled turn is a decision, not a fault to retry.
func TestTheCallersCancellationIsNotAHeaderTimeout(t *testing.T) {
	s, hits := muteServer(t)
	c := New("p", s.URL, nil)
	c.ResponseHeaderTimeout = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Post(ctx, "/x", nil)
	var te *Error
	if !errors.As(err, &te) || te.Retryable() || hits.Load() != 1 {
		t.Fatalf("err=%#v hits=%d, want one attempt and a non-retryable cancellation", err, hits.Load())
	}
}

// TestDefaultsBoundEveryEndpoint: a hosted and a loopback client both ship with
// a bound, and the loopback one honours slow prefill.
func TestDefaultsBoundEveryEndpoint(t *testing.T) {
	if got := New("p", "https://api.example.com/v1", nil).ResponseHeaderTimeout; got != DefaultHostedHeaderTimeout || got <= 0 {
		t.Fatalf("hosted = %s", got)
	}
	if got := New("p", "http://127.0.0.1:8080/v1", nil).ResponseHeaderTimeout; got != DefaultFirstTokenTimeout {
		t.Fatalf("loopback = %s, want the first-token budget", got)
	}
}

// TestPreflightStreamReplaysWhatItInspected and rejects before content.
func TestPreflightStreamJudgesOnlyThePreamble(t *testing.T) {
	judge := func(frame string) (bool, *Error) {
		name, _ := FrameFields(frame)
		switch name {
		case "ping":
			return true, nil
		case "error":
			return false, StreamFailure("p", "boom", 0, true)
		}
		return false, nil
	}
	resp := func(body string) *http.Response {
		return &http.Response{Body: io.NopCloser(strings.NewReader(body))}
	}

	accepted, failure := PreflightStream(resp("event: ping\ndata: {}\n\nevent: text\ndata: a\n\nevent: error\ndata: b\n\n"), "p", time.Second, judge)
	if failure != nil {
		t.Fatalf("an error after content was judged: %v", failure)
	}
	got, _ := io.ReadAll(accepted.Body)
	if !strings.Contains(string(got), "event: ping") || !strings.HasSuffix(string(got), "data: b\n\n") {
		t.Fatalf("replay lost bytes: %q", got)
	}

	if _, failure := PreflightStream(resp("event: ping\ndata: {}\n\nevent: error\ndata: b\n\n"), "p", time.Second, judge); failure == nil || !failure.Retryable() {
		t.Fatalf("an error in the preamble was not rejected: %v", failure)
	}
}

// TestPreflightStreamIsBoundedWhenTheServerGoesMute: headers sent, then silence.
func TestPreflightStreamIsBoundedWhenTheServerGoesMute(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	start := time.Now()
	_, failure := PreflightStream(&http.Response{Body: pr}, "p", 40*time.Millisecond,
		func(string) (bool, *Error) { return true, nil })
	if failure == nil || !failure.Retryable() {
		t.Fatalf("failure = %v", failure)
	}
	var stalled *ErrStalled
	if !errors.As(failure, &stalled) || time.Since(start) > 5*time.Second {
		t.Fatalf("failure = %v after %s", failure, time.Since(start))
	}
}
