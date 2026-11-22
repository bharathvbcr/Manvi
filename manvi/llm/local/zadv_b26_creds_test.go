package local

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/credentials"
)

// Adversarial pass on b26-creds. Every test here PASSES when the change holds.

// advB26Sink is a second listener that records every Authorization header and
// every request path it is sent: the place a redirect would carry a key to.
type advB26Sink struct {
	*httptest.Server
	mu   sync.Mutex
	hits []string
}

func newAdvB26Sink(t *testing.T) *advB26Sink {
	t.Helper()
	s := &advB26Sink{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits = append(s.hits, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization")+" x-api-key="+r.Header.Get("X-Api-Key"))
		s.mu.Unlock()
		_, _ = w.Write([]byte(`{"data":[{"id":"sink-model"}],"version":"9"}`))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *advB26Sink) leaked(key string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, h := range s.hits {
		if strings.Contains(h, key) {
			out = append(out, h)
		}
	}
	return out
}

func advB26Key(source string) func() (credentials.Secret, error) {
	return func() (credentials.Secret, error) {
		return credentials.NewSecret("sk-advb26-sentinel", source), nil
	}
}

// TestAdvB26CredsNamedEndpointRedirectDoesNotCarryTheKey: a NAMED endpoint that
// 302/307/308-redirects each probe URL to a different origin must not deliver
// the key there, on any of the requests a scan makes.
func TestAdvB26CredsNamedEndpointRedirectDoesNotCarryTheKey(t *testing.T) {
	for _, source := range []string{"LOCAL_API_KEY", "OPENAI_API_KEY"} {
		for _, code := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(fmt.Sprintf("%s/%d", source, code), func(t *testing.T) {
				sink := newAdvB26Sink(t)
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, sink.URL+r.URL.Path, code)
				}))
				defer origin.Close()
				Scan(context.Background(), ScanOptions{
					Endpoints:    []Endpoint{{BaseURL: origin.URL + "/v1", Named: true}},
					Credential:   advB26Key(source),
					Capabilities: true,
					Timeout:      5 * time.Second,
				})
				if got := sink.leaked("sk-advb26-sentinel"); len(got) > 0 {
					t.Fatalf("a redirect from a named endpoint carried the key to another origin: %q", got)
				}
			})
		}
	}
}

// TestAdvB26CredsRedirectOnEveryProbePath: the listing answers 200 itself, but
// each follow-up probe (identify, llama.cpp props/health, ollama show) redirects
// away. Those use the absolute-URL path rather than the listing path.
func TestAdvB26CredsRedirectOnEveryProbePath(t *testing.T) {
	sink := newAdvB26Sink(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		http.Redirect(w, r, sink.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	Scan(context.Background(), ScanOptions{
		Endpoints:    []Endpoint{{BaseURL: origin.URL + "/v1", Named: true}},
		Credential:   advB26Key("LOCAL_API_KEY"),
		Capabilities: true,
		Timeout:      5 * time.Second,
	})
	if got := sink.leaked("sk-advb26-sentinel"); len(got) > 0 {
		t.Fatalf("a probe path redirected off-origin and the key followed: %q", got)
	}
}

// TestAdvB26CredsOnlyNamedWins: a Named endpoint listed beside unnamed ones
// gets the key and the others get none, in the same scan.
func TestAdvB26CredsOnlyNamedWins(t *testing.T) {
	named, unnamed := newAuthRecorder(t, http.StatusOK), newAuthRecorder(t, http.StatusOK)
	Scan(context.Background(), ScanOptions{
		Endpoints: []Endpoint{
			{BaseURL: unnamed.URL + "/v1"},
			{BaseURL: named.URL + "/v1", Named: true},
		},
		Credential: advB26Key("LOCAL_API_KEY"),
		Timeout:    5 * time.Second,
	})
	for _, h := range unnamed.headers() {
		if h != "" {
			t.Fatalf("unnamed endpoint was sent %q", h)
		}
	}
	if h := named.headers(); len(h) == 0 || h[0] != "Bearer sk-advb26-sentinel" {
		t.Fatalf("named endpoint headers = %q", h)
	}
}

// TestAdvB26CredsWellKnownEndpointsAreNeverNamed: the shipped guesses must not
// carry Named.
func TestAdvB26CredsWellKnownEndpointsAreNeverNamed(t *testing.T) {
	for _, ep := range WellKnownEndpoints() {
		if ep.Named {
			t.Fatalf("shipped guess %s is Named", ep.BaseURL)
		}
	}
}

// TestAdvB26CredsBorrowedKeyRefusedOffMachineForEverySpelling drives the
// adapter's own header function with OPENAI_API_KEY against hostile spellings
// of a loopback address. A refusal is the right answer for each of these hosts.
func TestAdvB26CredsBorrowedKeyRefusedOffMachineForEverySpelling(t *testing.T) {
	remote := []string{
		"http://gpu.lan:8000/v1",
		"http://localhost.evil.test/v1",
		"http://127.0.0.1.evil.test/v1",
		"http://localhost@evil.test:80/v1",
		"http://127.0.0.1:80@evil.test/v1",
		"http://evil.test/v1?h=127.0.0.1",
		"http://evil.test/v1#@localhost",
		"http://evil.test/localhost/v1",
		"https://evil.test:443/v1",
		"http://[2001:db8::1]:8000/v1",
		"http://127.0.0.1:8000/v1\x00@evil.test",
		"http://localhost:80%40evil.test/v1",
		"http://evil.test\\@localhost/v1",
	}
	for _, base := range remote {
		t.Run(base, func(t *testing.T) {
			a := New(Config{BaseURL: base}, advB26Key("OPENAI_API_KEY"))
			h, err := a.Client().Header()
			if err == nil {
				t.Fatalf("a borrowed key was released for %q: %v", base, h)
			}
			if strings.Contains(err.Error(), "sk-advb26-sentinel") {
				t.Fatalf("the refusal quotes the key: %v", err)
			}
		})
	}
}

// TestAdvB26CredsBorrowedKeyAllowedOnlyOnLoopbackSpellings: the control.
func TestAdvB26CredsBorrowedKeyAllowedOnlyOnLoopbackSpellings(t *testing.T) {
	for _, base := range []string{
		"http://127.0.0.1:8000/v1", "http://localhost:8000/v1", "http://[::1]:8000/v1", "http://127.0.0.2:8000/v1",
	} {
		a := New(Config{BaseURL: base}, advB26Key("OPENAI_API_KEY"))
		if _, err := a.Client().Header(); err != nil {
			t.Errorf("loopback %s refused: %v", base, err)
		}
	}
}

// TestAdvB26CredsEmptyBaseURLNeverNamesTheShippedDefault: a Named endpoint with
// an empty address must not be treated as the shipped default address.
func TestAdvB26CredsResolveWithEmptyDeclaredIsStillUnscanned(t *testing.T) {
	res := ResolveEndpoint(context.Background(), ResolveOptions{Declared: "", DeclaredByOperator: true})
	if res.BaseURL != "" {
		t.Fatalf("an operator-declared EMPTY address resolved to %q: the harness filled in an address the operator never typed, and it is now Named", res.BaseURL)
	}
}

// ---- sanitising ----

func advB26ListenerWith(t *testing.T, ids []string, version string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			entries := make([]map[string]any, 0, len(ids))
			for _, id := range ids {
				entries = append(entries, map[string]any{"id": id})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
		case "/api/version":
			_ = json.NewEncoder(w).Encode(map[string]any{"version": version})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// advB26Hostile is a battery of strings a terminal or a reader should never be
// shown as live control. Names say why each is here.
var advB26Hostile = map[string]string{
	"esc-clear":    "m\x1b[2Jodel",
	"csi-sgr":      "m\x1b[31mred\x1b[0m",
	"osc8-link":    "m\x1b]8;;https://evil.test\x07click\x1b]8;;\x07",
	"osc8-st":      "m\x1b]8;;https://evil.test\x1b\\click",
	"osc52-clip":   "m\x1b]52;c;AAAA\x07",
	"bidi-rlo":     "gpt\u202Eknil\u202C",
	"bidi-isolate": "a\u2066b\u2069c",
	"line-sep":     "a\u2028FORGED LINE",
	"para-sep":     "a\u2029FORGED PARA",
	"nul":          "a\x00b",
	"c1-nel":       "a\u0085b",
	"c1-csi":       "a\u009b31mb",
	"del":          "a\x7fb",
	"newline":      "a\nFORGED LINE",
	"cr-overwrite": "good\rbad",
	"zero-width":   "a\u200Bb",
	"tag-chars":    "a\U000E0041b",
	"alm":          "a\u061Cb",
	"lrm":          "a\u200Eb",
	"invalid-utf8": "a\xff\xfeb",
}

func advB26Dangerous(s string) (rune, bool) {
	if !utf8.ValidString(s) {
		return utf8.RuneError, true
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
			// escaped separately by listenerString; a raw one is dangerous
			return r, true
		case unicode.IsControl(r), r == 0x2028, r == 0x2029:
			return r, true
		case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F, r == 0x061C:
			return r, true
		case unicode.Is(unicode.Cf, r):
			return r, true
		}
	}
	return 0, false
}

// TestAdvB26CredsModelIdsAreNeutralisedAtTheDecoder: every hostile id must come
// out of Scan with no live control, line break, bidi or format rune.
func TestAdvB26CredsModelIdsAreNeutralisedAtTheDecoder(t *testing.T) {
	for name, hostile := range advB26Hostile {
		t.Run(name, func(t *testing.T) {
			srv := advB26ListenerWith(t, []string{hostile, "plain:7b"}, "0.1")
			servers := Scan(context.Background(), ScanOptions{Endpoints: endpointsOf(srv.URL), Timeout: 5 * time.Second})
			if len(servers) != 1 {
				t.Fatalf("found %d servers", len(servers))
			}
			for _, m := range servers[0].Models {
				if r, bad := advB26Dangerous(m.ID); bad {
					t.Errorf("model id %q still carries U+%04X", m.ID, r)
				}
			}
		})
	}
}

// TestAdvB26CredsVersionIsNeutralised: the Ollama version string is the other
// listener-chosen string that reaches Describe.
func TestAdvB26CredsVersionIsNeutralised(t *testing.T) {
	for name, hostile := range advB26Hostile {
		t.Run(name, func(t *testing.T) {
			srv := advB26ListenerWith(t, []string{"m"}, hostile)
			servers := Scan(context.Background(), ScanOptions{Endpoints: endpointsOf(srv.URL), Timeout: 5 * time.Second})
			if len(servers) != 1 {
				t.Fatalf("found %d servers", len(servers))
			}
			if r, bad := advB26Dangerous(servers[0].Version + servers[0].Describe()); bad {
				t.Errorf("version %q / Describe %q carries U+%04X", servers[0].Version, servers[0].Describe(), r)
			}
		})
	}
}

// TestAdvB26CredsModelIdLengthIsBounded: a 1 MiB id is accepted verbatim today.
// An id printed on one line of a listing, a doctor row and every error that
// quotes it should be bounded.
func TestAdvB26CredsModelIdLengthIsBounded(t *testing.T) {
	const limit = 1024
	srv := advB26ListenerWith(t, []string{strings.Repeat("A", 1<<20), "ok"}, strings.Repeat("9", 1<<20))
	servers := Scan(context.Background(), ScanOptions{Endpoints: endpointsOf(srv.URL), Timeout: 5 * time.Second})
	if len(servers) != 1 {
		t.Fatalf("found %d servers", len(servers))
	}
	for _, m := range servers[0].Models {
		if len(m.ID) > limit {
			t.Errorf("a %d byte model id passed through unbounded (limit %d)", len(m.ID), limit)
		}
	}
	if len(servers[0].Version) > limit {
		t.Errorf("a %d byte version passed through unbounded (limit %d)", len(servers[0].Version), limit)
	}
}

// TestAdvB26CredsErrorBodyFromListenerIsNeutralised: a non-2xx body from the
// listener is quoted in the transport error that callers print.
func TestAdvB26CredsErrorBodyFromListenerIsNeutralised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom \x1b]52;c;AAAA\x07 \x1b[2J \u202E"))
	}))
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL + "/v1", DiscoveryTimeout: 2 * time.Second}, nil)
	_, err := a.Survey(context.Background(), false)
	if err == nil {
		t.Fatal("expected an error from a 500 listing")
	}
	if r, bad := advB26Dangerous(err.Error()); bad {
		t.Errorf("error text quotes a listener byte U+%04X unsanitised: %q", r, err.Error())
	}
}
