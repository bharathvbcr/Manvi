package local

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// The guess made for a server that did not answer is kept for the short retry,
// not the discovery TTL, so a server that recovers is read again soon.
func TestAnUnansweredProbeIsRetriedSoonNotAfterTheTTL(t *testing.T) {
	const model = "qwen3.5:4b"
	var hang atomic.Bool
	hang.Store(true)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"id": model}}})
		case "/api/show":
			if hang.Load() {
				select {
				case <-r.Context().Done():
				case <-release:
				}
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"model_info": map[string]any{"qwen3_5.context_length": 262144}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	defer close(release)

	a := New(Config{BaseURL: srv.URL + "/v1", ContextWindow: 40000, DiscoveryTimeout: 300 * time.Millisecond, DiscoveryTTL: time.Hour}, noCredential)
	now := time.Now()
	a.now = func() time.Time { return now }

	if dims := a.Dimensions(context.Background(), model); !dims.ProbeFailed || dims.Discovered() {
		t.Fatalf("the unanswered probe was not recorded: %+v", dims)
	}
	if again := a.Dimensions(context.Background(), model); !again.ProbeFailed {
		t.Fatalf("a second call inside the retry window lost the record: %+v", again)
	}
	hang.Store(false)
	now = now.Add(unprobedRetry + time.Second)
	recovered := a.Dimensions(context.Background(), model)
	if recovered.ContextWindow != 262144 || !recovered.Discovered() || recovered.ProbeFailed {
		t.Fatalf("a recovered server was still reported from the cached guess: %+v", recovered)
	}
}
