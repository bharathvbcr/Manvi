package local

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// dropServer accepts connections and closes them at once: a server that is up
// as a port and answers nothing. hits counts connections.
func advDropServer(t *testing.T) (url string, hits *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hits = &atomic.Int64{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String(), hits
}

// A server that is permanently down is re-probed each unprobedRetry forever.
// Over a simulated hour of callers, the probe load must back off, not stay flat.
func TestAdvB31OpsPermanentlyDownServerProbeLoadIsBounded(t *testing.T) {
	url, hits := advDropServer(t)
	a := New(Config{BaseURL: url + "/v1", ContextWindow: 40000, DiscoveryTimeout: 200 * time.Millisecond, DiscoveryTTL: time.Hour}, noCredential)
	now := time.Now()
	a.now = func() time.Time { return now }

	const model = "m"
	a.Dimensions(context.Background(), model)
	perRound := hits.Load()
	if perRound == 0 {
		t.Fatal("the probe never reached the server")
	}
	const rounds = 30 // 30 expiries of unprobedRetry+1s: five simulated minutes
	for range rounds {
		now = now.Add(unprobedRetry + time.Second)
		a.Dimensions(context.Background(), model)
	}
	total := hits.Load()
	if total >= perRound*(rounds+1)/2 {
		t.Fatalf("%d connections over %d rounds (%d per round): a permanently down server is probed every %s with no backoff",
			total, rounds+1, perRound, unprobedRetry)
	}
}

// An HTTP status is treated as an answer. A 503 or 429 from an overloaded
// server says nothing about what it publishes, yet it is then cached for the
// whole DiscoveryTTL as "publishes no window".
func TestAdvB31OpsOverloadedServerIsRetriedSoon(t *testing.T) {
	const model = "qwen3.5:4b"
	var status atomic.Int64
	status.Store(http.StatusServiceUnavailable)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := int(status.Load()); s != 200 {
			w.WriteHeader(s)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"qwen3_5.context_length": 262144}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	for _, code := range []int{http.StatusServiceUnavailable, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status.Store(int64(code))
		a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 40000, DiscoveryTimeout: time.Second, DiscoveryTTL: time.Hour}, noCredential)
		now := time.Now()
		a.now = func() time.Time { return now }
		first := a.Dimensions(context.Background(), model)
		status.Store(200)
		now = now.Add(unprobedRetry + time.Second)
		got := a.Dimensions(context.Background(), model)
		if !got.Discovered() {
			t.Errorf("status %d: after the server recovered, a call %s later still got the cached guess (first: ProbeFailed=%v): %+v",
				code, unprobedRetry+time.Second, first.ProbeFailed, got)
		}
	}
}

// 64 concurrent callers against a hung server: one probe, and every caller is
// told the window is unknown rather than that the server publishes none.
func TestAdvB31OpsConcurrentDimensionsAgainstAHungServer(t *testing.T) {
	const model = "qwen3.5:4b"
	var shows atomic.Int64
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			shows.Add(1)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	defer close(release)
	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 40000, DiscoveryTimeout: 400 * time.Millisecond, DiscoveryTTL: time.Hour}, noCredential)
	var wg sync.WaitGroup
	results := make([]Dimensions, 64)
	for i := range results {
		wg.Add(1)
		go func() { defer wg.Done(); results[i] = a.Dimensions(context.Background(), model) }()
	}
	wg.Wait()
	if n := shows.Load(); n != 1 {
		t.Errorf("%d /api/show probes for one cold model, want 1", n)
	}
	for i, d := range results {
		if !d.ProbeFailed || d.Discovered() || d.ContextWindow != 40000 {
			t.Fatalf("caller %d got %+v, want the declaration marked ProbeFailed", i, d)
		}
	}
}
