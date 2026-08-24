package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"manvi/llm/adaptertest"
)

// TestARetryableFailureIsFoundWhateverTheLineEnding.
//
// preflight split frames on the literal "\n\n", which never occurs in a
// CRLF-framed stream — so the same body from the same server was retried three
// times with LF endings and once with CRLF, and the four-attempt policy this
// whole file exists to reach was skipped on nothing but a line ending. The
// decoder that reads the very same bytes a moment later accepts both, because
// transport.SSE trims "\r\n" off every line; two parsers disagreeing about
// where a frame ends is a defect whether or not a live endpoint currently
// exercises it.
//
// The LF case is kept beside the CRLF one deliberately: it is the control that
// says the CRLF failure was about framing and not about the body.
func TestARetryableFailureIsFoundWhateverTheLineEnding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ending string
	}{
		{name: "LF, the shape this was written against", ending: "\n"},
		{name: "CRLF, which the same reader downstream accepts", ending: "\r\n"},
		{name: "a blank line whose ending differs from the line before it", ending: "mixed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := reframe(overloadedFrames, tc.ending)
			server, calls := flakyServerFramed(t, 2, reframe(happyStream, tc.ending), body)
			adapter := New(server.URL, adaptertest.Secret("k"))
			adapter.client.Retry.BaseDelay = 0

			stream, err := adapter.Stream(adaptertest.Ctx(), request())
			if err != nil {
				t.Fatalf("a transient in-stream failure was fatal: %v", err)
			}
			if _, _, err := adaptertest.Drain(stream); err != nil {
				t.Fatal(err)
			}
			if got := atomic.LoadInt32(calls); got != 3 {
				t.Errorf("server saw %d request(s), want 3 — a retryable overload framed "+
					"with %q line endings was not seen by the preflight scan",
					got, tc.ending)
			}
		})
	}
}

// flakyServerFramed fails the first n requests with the given failure body and
// then serves the happy one. It is flakyServer with the failure body as a
// parameter, because what is under test here is the framing of that body.
func flakyServerFramed(t *testing.T, failures int32, happy, failure string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		out := happy
		if n <= failures {
			out = failure
		}
		// Written nine bytes at a time, so a frame terminator is routinely
		// split across two reads and the scanner has to reassemble it.
		for i := 0; i < len(out); i += 9 {
			end := i + 9
			if end > len(out) {
				end = len(out)
			}
			w.Write([]byte(out[i:end]))
			flusher.Flush()
		}
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

// reframe rewrites an LF-framed SSE body with the given line ending. "mixed"
// gives content lines CRLF and the blank line that ends a frame a bare LF,
// which is the case a scanner looking for one fixed four-byte sequence misses.
func reframe(body, ending string) string {
	switch ending {
	case "\n":
		return body
	case "mixed":
		out := strings.ReplaceAll(body, "\n", "\r\n")
		return strings.ReplaceAll(out, "\r\n\r\n", "\r\n\n")
	default:
		return strings.ReplaceAll(body, "\n", ending)
	}
}
