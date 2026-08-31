package adaptertest

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestServerRecordsConcurrentRequests drives a server from several goroutines
// at once while the test goroutine reads the recordings. Under -race this
// fails if the recording slices are touched without the mutex, which is the
// shape any test of parallel provider calls would hit.
func TestServerRecordsConcurrentRequests(t *testing.T) {
	const n = 8
	s := NewServer(t, "data: hello\n\n")

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, err := http.NewRequest(http.MethodPost, s.URL, strings.NewReader("body"))
			if err != nil {
				t.Error(err)
				return
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			// Read while other requests are still being recorded.
			s.Requests()
			s.Headers()
		}(i)
	}
	wg.Wait()

	if got := len(s.Requests()); got != n {
		t.Errorf("recorded %d requests, want %d", got, n)
	}
	if got := len(s.Headers()); got != n {
		t.Errorf("recorded %d header sets, want %d", got, n)
	}
	for i, body := range s.Requests() {
		if body != "body" {
			t.Errorf("request %d body = %q, want %q", i, body, "body")
		}
	}
}

// TestRequestsCopy checks the accessors hand back copies: a caller that
// mutates the returned slice must not corrupt the server's own record.
func TestRequestsCopy(t *testing.T) {
	s := NewStatusServer(t, http.StatusTooManyRequests, "slow down")

	resp, err := http.Post(s.URL, "application/json", strings.NewReader("first"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	got := s.Requests()
	got[0] = "clobbered"
	if again := s.Requests(); again[0] != "first" {
		t.Errorf("Requests() handed out the live slice: %q", again[0])
	}

	hdrs := s.Headers()
	hdrs[0] = nil
	if again := s.Headers(); again[0] == nil {
		t.Error("Headers() handed out the live slice")
	}
}
