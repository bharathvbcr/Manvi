package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// recorder is a server that remembers what reached it.
type recorder struct {
	*httptest.Server
	mu   sync.Mutex
	hits []recorded
}

type recorded struct {
	path, key, body string
}

func newRecorder(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		rec.mu.Lock()
		rec.hits = append(rec.hits, recorded{path: r.URL.Path, key: r.Header.Get("x-api-key"), body: string(raw)})
		rec.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(rec.Close)
	return rec
}

func (r *recorder) seen() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.hits...)
}

func keyed(provider, base string) *Client {
	c := New(provider, base, func() (http.Header, error) {
		return http.Header{"X-Api-Key": {"sk-FIXTURE-not-a-real-key"}}, nil
	})
	c.Retry.BaseDelay = 0
	return c
}

// net/http strips only Authorization, Cookie and WWW-Authenticate when a
// redirect leaves the host, so x-api-key and x-goog-api-key followed a 307/308
// to any other origin, with the request body.
func TestACrossOriginRedirectIsRefusedAndSendsNothing(t *testing.T) {
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			other := newRecorder(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("{}")) })
			origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.URL+"/v1/messages", status)
			})
			c := keyed("fixture", origin.URL)

			resp, err := c.Post(context.Background(), "/v1/messages", map[string]string{"secret": "prompt body"})
			if err == nil {
				_ = resp.Body.Close()
				t.Fatal("a cross-origin redirect was followed")
			}
			var refused *ErrCrossOriginRedirect
			if !errors.As(err, &refused) {
				t.Fatalf("error = %v, want an *ErrCrossOriginRedirect", err)
			}
			var terr *Error
			if !errors.As(err, &terr) || terr.Retryable() {
				t.Errorf("a refused redirect must be a permanent failure, got %#v", err)
			}
			if n := len(other.seen()); n != 0 {
				t.Errorf("the other origin received %d request(s): %+v", n, other.seen())
			}
			if n := len(origin.seen()); n != 1 {
				t.Errorf("the origin was asked %d times, want 1: a refusal is not retried", n)
			}
		})
	}
}

// TestOriginIsSchemeHostAndPort: an https to http downgrade on the same host
// is another origin, as is another port, and a default port is the same origin
// as its spelled-out form.
func TestOriginIsSchemeHostAndPort(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"https://api.example.com/v1", "https://api.example.com/v2?x=1", true},
		{"https://api.example.com", "https://API.example.com:443/", true},
		{"http://localhost:8080/a", "http://localhost:8080/b", true},
		{"https://api.example.com", "http://api.example.com", false},
		{"http://localhost:8080", "http://localhost:8081", false},
		{"https://api.example.com", "https://evil.example.com", false},
		{"https://api.example.com", "https://api.example.com.evil.test", false},
	}
	for _, c := range cases {
		a, err := url.Parse(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := url.Parse(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameOrigin(a, b); got != c.same {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

// TestAProbeRefusesACrossOriginRedirect: Probe builds its own request but goes
// through the same client.
func TestAProbeRefusesACrossOriginRedirect(t *testing.T) {
	other := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {})
	origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/props", http.StatusTemporaryRedirect)
	})
	c := keyed("fixture", origin.URL)
	if resp, err := c.Probe(context.Background(), http.MethodGet, origin.URL+"/props", nil); err == nil {
		_ = resp.Body.Close()
		t.Fatal("a probe followed a cross-origin redirect")
	}
	if n := len(other.seen()); n != 0 {
		t.Errorf("the other origin received %d request(s)", n)
	}
}

// TestASameOriginRedirectStillCarriesTheBodyAndKey: the behaviour that was
// there before is the behaviour that stays.
func TestASameOriginRedirectStillCarriesTheBodyAndKey(t *testing.T) {
	origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusTemporaryRedirect)
			return
		}
		_, _ = w.Write([]byte("{}"))
	})
	c := keyed("fixture", origin.URL)
	resp, err := c.Post(context.Background(), "/old", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("a same-origin redirect failed: %v", err)
	}
	_ = resp.Body.Close()
	hits := origin.seen()
	if len(hits) != 2 || hits[1].path != "/new" {
		t.Fatalf("hits = %+v, want /old then /new", hits)
	}
	if hits[1].key != "sk-FIXTURE-not-a-real-key" || hits[1].body != `{"k":"v"}` {
		t.Errorf("same-origin redirect lost the key or body: %+v", hits[1])
	}
}

func TestOriginNormalisationEdges(t *testing.T) {
	cases := []struct {
		a, b string
		same bool
	}{
		{"http://h/x", "http://h:80/y", true},
		{"http://[::1]:8080/x", "http://[::1]:8080/y", true},
		{"http://[::1]/x", "http://[::1]:80/y", true},
		{"http://localhost:1/x", "http://127.0.0.1:1/x", false},
		{"https://u:p@h/x", "https://h/x", true},
		{"https://h/x", "https://h:8443/x", false},
		{"HTTPS://h/x", "https://h/x", true},
		{"https://h/x", "https://h@evil.example/x", false},
		{"https://h/x", "ftp://h/x", false},
	}
	for _, c := range cases {
		a, err := url.Parse(c.a)
		if err != nil {
			t.Fatal(err)
		}
		b, err := url.Parse(c.b)
		if err != nil {
			t.Fatal(err)
		}
		if got := sameOrigin(a, b); got != c.same {
			t.Errorf("sameOrigin(%s, %s) = %v, want %v", c.a, c.b, got, c.same)
		}
	}
}

// A chain of nine same-origin hops is followed; the limit is on the tenth.
func TestANineHopSameOriginChainIsFollowed(t *testing.T) {
	origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		var k int
		if _, err := fmt.Sscanf(r.URL.Path, "/hop/%d", &k); err != nil || k >= 9 {
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/hop/%d", k+1), http.StatusTemporaryRedirect)
	})
	c := keyed("fixture", origin.URL)
	resp, err := c.Post(context.Background(), "/hop/0", map[string]string{"a": "b"})
	if err != nil {
		t.Fatalf("a nine-hop chain failed: %v", err)
	}
	_ = resp.Body.Close()
}

// A same-origin redirect loop is a misconfigured endpoint, not a transient
// fault: it is reported as a typed permanent error after one chain of ten hops,
// not retried four times over.
func TestARedirectLoopIsPermanentAndNotRetried(t *testing.T) {
	origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loop", http.StatusTemporaryRedirect)
	})
	c := keyed("fixture", origin.URL)
	resp, err := c.Post(context.Background(), "/loop", map[string]string{})
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("an endless redirect loop was followed")
	}
	var loop *ErrTooManyRedirects
	if !errors.As(err, &loop) {
		t.Fatalf("error = %v, want *ErrTooManyRedirects", err)
	}
	var terr *Error
	if !errors.As(err, &terr) || terr.Retryable() {
		t.Errorf("a redirect loop must be permanent, got %#v", err)
	}
	if n := len(origin.seen()); n > 11 {
		t.Errorf("origin saw %d requests, want one chain of at most 11", n)
	}
}

// net/http wraps a redirect failure in a *url.Error whose text is
// `Post "<full URL>"`, query string included. The refusal error keeps its own
// typed error instead, so a query string never reaches a log line.
func TestARefusalDoesNotCarryAQueryString(t *testing.T) {
	other := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {})
	origin := newRecorder(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/p?token=LOCATION-SECRET", http.StatusTemporaryRedirect)
	})
	c := keyed("fixture", origin.URL)
	resp, err := c.Post(context.Background(), "/v1/messages?key=REQUEST-SECRET", map[string]string{"a": "b"})
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("a cross-origin redirect was followed")
	}
	var refused *ErrCrossOriginRedirect
	if !errors.As(err, &refused) {
		t.Fatalf("error = %v, want *ErrCrossOriginRedirect", err)
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		t.Errorf("the error still wraps net/http's *url.Error: %v", err)
	}
	for _, secret := range []string{"REQUEST-SECRET", "LOCATION-SECRET"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error text carries %s: %v", secret, err)
		}
	}
}
