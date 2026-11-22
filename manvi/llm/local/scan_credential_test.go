package local

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

// authRecorder is a model server that remembers every Authorization header it
// was sent, on any path.
type authRecorder struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string
}

func (a *authRecorder) headers() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func newAuthRecorder(t *testing.T, status int) *authRecorder {
	t.Helper()
	rec := &authRecorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.mu.Lock()
		rec.seen = append(rec.seen, r.Header.Get("Authorization"))
		rec.mu.Unlock()
		if r.URL.Path == "/v1/models" {
			if status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"some-model"}]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(rec.Close)
	return rec
}

// borrowedKey is the resolver the "local" provider has when only OPENAI_API_KEY
// is exported: a real key, set for a different service.
func borrowedKey() (credentials.Secret, error) {
	return credentials.NewSecret("sk-not-a-real-key", "OPENAI_API_KEY"), nil
}

// TestAScanSendsNoCredentialToAnAddressTheOperatorDidNotName is the leak the
// scan had: the resolved key was attached to every candidate it probed, so an
// exported OPENAI_API_KEY went to whatever listened on five well-known ports.
func TestAScanSendsNoCredentialToAnAddressTheOperatorDidNotName(t *testing.T) {
	srv := newAuthRecorder(t, http.StatusOK)
	servers := Scan(context.Background(), ScanOptions{
		Endpoints:    endpointsOf(srv.URL),
		Credential:   borrowedKey,
		Capabilities: true,
		Timeout:      5 * time.Second,
	})
	if len(servers) != 1 {
		t.Fatalf("found %d servers, want the one that answered", len(servers))
	}
	for _, h := range srv.headers() {
		if h != "" {
			t.Fatalf("an address nobody named was sent Authorization %q", h)
		}
	}
	if len(srv.headers()) == 0 {
		t.Fatal("the server saw no request, so the assertion above proved nothing")
	}
}
