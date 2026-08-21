package local

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Capability is reached from the agent loop and from every fan-out sub-agent,
// so the dimension cache is genuinely concurrent. It must issue one probe, not
// one per caller — a fan-out that each probed would turn a cold start into a
// thundering herd against a server that is already the bottleneck.
func TestConcurrentDimensionLookupsProbeOnce(t *testing.T) {
	const model = "m"
	var showCalls, listCalls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			atomic.AddInt64(&listCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			atomic.AddInt64(&showCalls, 1)
			// A real /api/show against a loaded model is not instant. The delay
			// is what makes the stampede visible rather than accidentally
			// serialised by how fast an httptest handler answers: while the
			// first caller is inside the probe, every other caller finds the
			// cache still empty.
			time.Sleep(100 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model_info": map[string]any{"qwen3_5.context_length": 262144},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 32768, SupportsTools: true}, noCredential)

	// Deliberately cold. Warming the cache first and then counting probes
	// asserts the warm path, which was never the risk: the stampede happens on
	// the first fan-out of a session and again on every DiscoveryTTL expiry,
	// when every caller misses the cache at once.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if cap, ok := a.Capability(model); !ok || cap.ContextWindow != 262144 {
				t.Errorf("capability = %+v, ok=%v", cap, ok)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(&showCalls); got != 1 {
		t.Fatalf("64 cold concurrent lookups issued %d probes, want 1", got)
	}
	if got := atomic.LoadInt64(&listCalls); got != 1 {
		t.Fatalf("64 cold concurrent lookups issued %d model listings, want 1", got)
	}

	// And the warm path still costs nothing: the second fan-out is served from
	// the cache the first one filled.
	for i := 0; i < 16; i++ {
		if cap, ok := a.Capability(model); !ok || cap.ContextWindow != 262144 {
			t.Fatalf("warm capability = %+v, ok=%v", cap, ok)
		}
	}
	if got := atomic.LoadInt64(&showCalls); got != 1 {
		t.Fatalf("the warm path issued %d probes, want the original 1", got)
	}
}

// A server that is down must not make every concurrent caller wait for its own
// timeout. The negative result is cached like a positive one.
func TestConcurrentLookupsAgainstADeadServerDoNotStampede(t *testing.T) {
	var calls int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 4096, SupportsTools: true}, noCredential)

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := a.Capability("m"); ok {
				t.Error("a dead server reported a served model")
			}
		}()
	}
	wg.Wait()

	// One discovery attempt (with its retries), not thirty-two.
	if got := atomic.LoadInt64(&calls); got > 8 {
		t.Fatalf("a dead server drew %d requests from 32 concurrent lookups", got)
	}
}
