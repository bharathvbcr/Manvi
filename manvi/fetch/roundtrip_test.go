package fetch

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// The tests above check each rule on its own. These drive a real TLS server
// through the real transport, because a rule that holds in isolation and never
// runs on the live path is a rule that is not in force.
//
// The server listens on loopback, which this client refuses by design. So the
// address check is pointed at the server's own address for these tests only —
// stated here rather than hidden in a helper, because relaxing the central
// safety property to test the code around it is exactly the move that needs to
// be visible.

func serve(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)

	addrPort := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "https://"))
	c := New([]string{"docs.example"}, Limits{Timeout: 5 * time.Second})
	c.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{addrPort.Addr()}, nil
	}
	// Loopback stays blocked everywhere else; here the check is told this one
	// address is the server under test. Everything else keeps the real rule,
	// so a bug that let some other blocked range through would still fail.
	real := blockedAddr
	c.blocked = func(addr netip.Addr) (string, bool) {
		if addr == addrPort.Addr() {
			return "", false
		}
		return real(addr)
	}
	// The URL names no port, so the client dials 443; the test server is on an
	// ephemeral one. Only the port is redirected — the address check above has
	// already run, and every other rule is still the shipped one.
	c.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, addrPort.String())
	}
	c.http.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	return srv, c
}

func fetchPath(t *testing.T, c *Client, path string) (*Document, error) {
	t.Helper()
	return c.Fetch(context.Background(), "https://docs.example"+path)
}

func TestRoundTripExtractsAPage(t *testing.T) {
	_, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>Package fetch</title>
			<script>var tracking = 1;</script></head>
			<body><h1>Package fetch</h1><p>It fetches things.</p></body></html>`)
	})

	doc, err := fetchPath(t, c, "/doc")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if doc.Title != "Package fetch" {
		t.Errorf("title = %q", doc.Title)
	}
	if !strings.Contains(doc.Text, "It fetches things.") {
		t.Errorf("text = %q", doc.Text)
	}
	if strings.Contains(doc.Text, "tracking") {
		t.Errorf("the script leaked through the live path: %q", doc.Text)
	}
	if doc.Truncated {
		t.Error("a small page reported itself truncated")
	}
}

// The byte cap is enforced on the wire, and the result says the page is a
// prefix. A model told otherwise reasons about a document it did not read.
func TestRoundTripEnforcesTheByteCap(t *testing.T) {
	_, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<p>")
		for range 200 {
			fmt.Fprint(w, strings.Repeat("filler ", 1000))
		}
	})
	c.limits.MaxBytes = 4096

	doc, err := fetchPath(t, c, "/big")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !doc.Truncated {
		t.Fatal("a body past the cap did not report itself truncated")
	}
	if doc.Bytes > 4096 {
		t.Fatalf("read %d bytes against a 4096 cap", doc.Bytes)
	}
}

// A non-2xx is an error, not an empty document. A 404 body rendered as prose is
// a model reasoning about an error page.
func TestRoundTripRejectsNonSuccess(t *testing.T) {
	_, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	if _, err := fetchPath(t, c, "/missing"); err == nil ||
		!strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v, want the status reported", err)
	}
}

// Binary is refused rather than reduced to mojibake.
func TestRoundTripRefusesBinary(t *testing.T) {
	_, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	})
	if _, err := fetchPath(t, c, "/logo.png"); err == nil ||
		!strings.Contains(err.Error(), "not a text document") {
		t.Fatalf("err = %v, want a refusal naming the type", err)
	}
}

// A server that never answers must not hold the turn open past the limit.
func TestRoundTripHonoursTheTimeout(t *testing.T) {
	_, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	c.http.Timeout = 300 * time.Millisecond

	start := time.Now()
	if _, err := fetchPath(t, c, "/hang"); err == nil {
		t.Fatal("a hanging server produced a document")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the fetch took %s against a 300ms timeout", elapsed)
	}
}

// A redirect to a host outside the allowlist is refused on the live path, not
// merely in the unit test of checkRedirect.
func TestRoundTripRefusesARedirectOffTheAllowlist(t *testing.T) {
	_, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/stolen", http.StatusFound)
	})
	if _, err := fetchPath(t, c, "/go"); err == nil ||
		!strings.Contains(err.Error(), "allowlist") {
		t.Fatalf("err = %v, want the redirect refused", err)
	}
}

// A redirect chain that never terminates is bounded by the harness.
func TestRoundTripBoundsARedirectLoop(t *testing.T) {
	var srv *httptest.Server
	srv, c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://docs.example/loop", http.StatusFound)
	})
	_ = srv
	if _, err := fetchPath(t, c, "/loop"); err == nil ||
		!strings.Contains(err.Error(), "redirect") {
		t.Fatalf("err = %v, want the loop bounded", err)
	}
}
