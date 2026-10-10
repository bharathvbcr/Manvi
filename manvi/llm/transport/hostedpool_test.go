package transport

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The hosted pool exists to change one number. Everything else — the proxy
// from the environment, the dialer, TLS, HTTP/2, every timeout — must be the
// default's, because those decide which requests reach a provider at all and
// how, and a pool that quietly dropped the environment proxy would take a
// corporate network offline.
func TestHostedClientsShareTheDefaultWithOnlyTheIdleBoundRaised(t *testing.T) {
	a := New("anthropic", "https://api.anthropic.com", nil)
	b := New("gemini", "https://generativelanguage.googleapis.com", nil)
	if a.HTTP.Transport != b.HTTP.Transport {
		t.Fatal("two hosted clients got separate pools; idle connections would not be shared")
	}
	got, ok := a.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("hosted transport is %T, want *http.Transport", a.HTTP.Transport)
	}
	stock := asTransport(t, http.DefaultTransport)
	if got == stock {
		t.Fatal("hosted clients still use http.DefaultTransport itself")
	}
	if got.MaxIdleConnsPerHost != hostedMaxIdleConnsPerHost {
		t.Errorf("MaxIdleConnsPerHost = %d, want %d", got.MaxIdleConnsPerHost, hostedMaxIdleConnsPerHost)
	}

	gv, sv := reflect.ValueOf(got).Elem(), reflect.ValueOf(stock).Elem()
	for i := 0; i < gv.NumField(); i++ {
		field := gv.Type().Field(i)
		// TLSNextProto is filled in by each transport's own HTTP/2 setup on
		// first use, pointing at that transport's own HTTP/2 pool, so it is
		// checked for what it means below rather than for equality.
		if !field.IsExported() || field.Name == "MaxIdleConnsPerHost" || field.Name == "TLSNextProto" {
			continue
		}
		g, s := gv.Field(i), sv.Field(i)
		switch g.Kind() {
		case reflect.Func:
			// A clone shares the default's functions — ProxyFromEnvironment,
			// the dialer's DialContext — by identity.
			if g.IsNil() != s.IsNil() || (!g.IsNil() && g.Pointer() != s.Pointer()) {
				t.Errorf("%s differs from http.DefaultTransport's", field.Name)
			}
		default:
			if !reflect.DeepEqual(g.Interface(), s.Interface()) {
				t.Errorf("%s = %v, http.DefaultTransport has %v", field.Name, g.Interface(), s.Interface())
			}
		}
	}
	if got.TLSNextProto != nil && got.TLSNextProto["h2"] == nil {
		// A non-nil map without h2, even an empty one, is the documented way
		// to turn HTTP/2 off.
		t.Error("TLSNextProto on the clone has no h2 entry, which disables HTTP/2")
	}
	if !got.ForceAttemptHTTP2 {
		t.Error("ForceAttemptHTTP2 lost in the clone")
	}

	// The loopback pool is unaffected.
	local := New("local", "http://127.0.0.1:8000/v1", nil)
	if local.HTTP.Transport != sharedLoopback() {
		t.Error("a loopback client no longer gets the loopback pool")
	}
}

// A program that replaced http.DefaultTransport chose what default clients
// use, and these clients were default clients before the shared pool existed.
func TestAReplacedDefaultTransportIsHonoured(t *testing.T) {
	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })

	custom := &countingRoundTripper{}
	http.DefaultTransport = custom
	c := New("anthropic", "https://api.anthropic.com", nil)
	if c.HTTP.Transport != http.RoundTripper(custom) {
		t.Errorf("transport = %T, want the replacement the program installed", c.HTTP.Transport)
	}

	// Including a replacement that is itself an *http.Transport: it carries
	// the program's own settings, which the shared clone of the stock
	// default would silently drop.
	own := &http.Transport{MaxIdleConnsPerHost: 1}
	http.DefaultTransport = own
	c = New("anthropic", "https://api.anthropic.com", nil)
	if c.HTTP.Transport != http.RoundTripper(own) {
		t.Errorf("transport = %v, want the program's own *http.Transport", c.HTTP.Transport)
	}

	http.DefaultTransport = saved
	c = New("anthropic", "https://api.anthropic.com", nil)
	if c.HTTP.Transport != sharedHosted() {
		t.Error("restoring the stock default did not restore the shared pool")
	}
}

type countingRoundTripper struct{ n atomic.Int64 }

func (c *countingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	c.n.Add(1)
	return nil, io.EOF
}

// handshakesPerRound drives rounds of concurrent requests through tr against
// an HTTP/1.1-only TLS server and reports how many connections — and so TLS
// handshakes — each round after the first opened.
// asTransport names what a round tripper actually is, instead of panicking
// with "interface conversion" when it is not the *http.Transport these tests
// measure.
func asTransport(tb testing.TB, rt http.RoundTripper) *http.Transport {
	tb.Helper()
	tr, ok := rt.(*http.Transport)
	if !ok {
		tb.Fatalf("round tripper is %T, want *http.Transport", rt)
	}
	return tr
}

func handshakesPerRound(tb testing.TB, tr *http.Transport, rounds, fanOut int) float64 {
	tb.Helper()
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	defer srv.Close()

	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tr = tr.Clone()
	tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	tr.Proxy = nil
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	round := func() {
		var wg sync.WaitGroup
		for i := 0; i < fanOut; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req, err := http.NewRequestWithContext(tb.Context(), http.MethodGet, srv.URL, nil)
				if err != nil {
					tb.Error(err)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					tb.Error(err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			}()
		}
		wg.Wait()
	}
	round()
	first := opened.Load()
	for i := 1; i < rounds; i++ {
		// A connection goes back to the pool from its read loop just after
		// its body closes, not before Close returns. Real rounds of a fan-out
		// are separated by a model's latency; without a pause this measures
		// that race instead of the pool.
		time.Sleep(20 * time.Millisecond)
		round()
	}
	return float64(opened.Load()-first) / float64(rounds-1)
}

// After the first round has opened its connections, a round of the same size
// should reuse them rather than re-handshake.
func TestTheHostedPoolKeepsAFanOutsConnections(t *testing.T) {
	const fanOut = 32
	got := handshakesPerRound(t, asTransport(t, sharedHosted()), 5, fanOut)
	stock := handshakesPerRound(t, asTransport(t, http.DefaultTransport), 5, fanOut)
	t.Logf("new TLS connections per round of %d concurrent requests: stock default %.1f, hosted pool %.1f",
		fanOut, stock, got)
	// Loose on purpose, so a loaded runner cannot fail it. The stock default
	// sits near fanOut-2.
	if got >= fanOut/2 {
		t.Errorf("hosted pool opened %.1f connections per warm round of %d, want close to 0", got, fanOut)
	}
}

func BenchmarkHostedFanOutHandshakes(b *testing.B) {
	for _, tc := range []struct {
		name string
		tr   *http.Transport
	}{
		{"stock-default", asTransport(b, http.DefaultTransport)},
		{"hosted-pool", asTransport(b, sharedHosted())},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var total float64
			for b.Loop() {
				total += handshakesPerRound(b, tc.tr, 4, 32)
			}
			b.ReportMetric(total/float64(b.N), "handshakes/round")
		})
	}
}
