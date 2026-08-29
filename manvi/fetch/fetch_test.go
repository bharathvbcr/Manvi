package fetch

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The URL this fetches is chosen by a model, and a model's choice is downstream
// of whatever it just read. So every test here treats the URL as hostile input
// and tries to reach somewhere the harness must not go.

// withResolver builds a client whose name lookups are scripted, because a test
// that depended on the system resolver would be testing DNS.
func withResolver(t *testing.T, allowed []string, table map[string][]string) *Client {
	t.Helper()
	c := New(allowed, Limits{})
	c.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		raw, ok := table[host]
		if !ok {
			return nil, fmt.Errorf("no such host: %s", host)
		}
		addrs := make([]netip.Addr, 0, len(raw))
		for _, s := range raw {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		return addrs, nil
	}
	return c
}

// The off switch, and the default. A harness whose operator has said nothing
// about network access does not have network access.
func TestNoAllowlistMeansNoFetching(t *testing.T) {
	for _, allowed := range [][]string{nil, {}, {""}, {"  ", "\t"}} {
		c := New(allowed, Limits{})
		if c.Enabled() {
			t.Fatalf("New(%q) produced an enabled client", allowed)
		}
		if _, err := c.Fetch(context.Background(), "https://go.dev/doc"); !errors.Is(err, ErrDisabled) {
			t.Fatalf("Fetch with allowlist %q = %v, want ErrDisabled", allowed, err)
		}
	}
}

// The address space this must never reach, one case per range. Each is a real
// place a server-side request forgery aims at.
func TestBlockedAddressRanges(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":       "loopback",
		"127.1.2.3":       "loopback",
		"::1":             "loopback",
		"0.0.0.0":         "unspecified",
		"::":              "unspecified",
		"10.1.2.3":        "private",
		"172.16.0.1":      "private",
		"192.168.1.1":     "private",
		"fd00::1":         "private",
		"169.254.169.254": "link-local — the cloud metadata endpoint",
		"fe80::1":         "link-local",
		"224.0.0.1":       "multicast",
		"ff02::1":         "multicast",
		"100.64.0.1":      "carrier-grade NAT",
		"192.0.2.1":       "documentation range",
		"198.18.0.1":      "benchmarking range",
		"203.0.113.5":     "documentation range",
		"240.0.0.1":       "reserved",
		"2001:db8::1":     "documentation range",
		"64:ff9b::1":      "NAT64",
		// The v4-mapped form of loopback, which reaches the same place while
		// looking like a different address family.
		"::ffff:127.0.0.1": "loopback via a v4-mapped address",
	}
	for addr, why := range cases {
		t.Run(addr, func(t *testing.T) {
			reason, blocked := blockedAddr(netip.MustParseAddr(addr))
			if !blocked {
				t.Fatalf("%s (%s) was not blocked", addr, why)
			}
			if reason == "" {
				t.Fatal("blocked without a reason; whoever hits this deserves to be told the rule")
			}
		})
	}
}

// And the addresses that must still work, or the feature is a refusal engine.
func TestPublicAddressesAreNotBlocked(t *testing.T) {
	for _, addr := range []string{"93.184.216.34", "1.1.1.1", "8.8.8.8", "2606:4700::1111"} {
		if reason, blocked := blockedAddr(netip.MustParseAddr(addr)); blocked {
			t.Errorf("%s was blocked as %q", addr, reason)
		}
	}
}

// A host that is not on the list is refused before anything is resolved.
func TestHostMustBeAllowed(t *testing.T) {
	c := withResolver(t, []string{"go.dev"}, map[string][]string{
		"evil.example": {"93.184.216.34"},
	})
	_, err := c.Fetch(context.Background(), "https://evil.example/x")
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("err = %v, want ErrNotAllowed", err)
	}
	// And the refusal names the list, so an operator can see what they set.
	if !strings.Contains(err.Error(), "go.dev") {
		t.Fatalf("the refusal did not name the allowlist: %v", err)
	}
}

// Subdomains of an allowed host are allowed; lookalikes are not. A suffix
// comparison on the raw string would have admitted "notgo.dev".
func TestAllowlistMatchesWholeLabels(t *testing.T) {
	c := New([]string{"go.dev"}, Limits{})
	allowed := []string{"go.dev", "pkg.go.dev", "deep.nested.go.dev", "GO.DEV"}
	for _, host := range allowed {
		if !c.allows(strings.ToLower(host)) {
			t.Errorf("%q should be allowed by go.dev", host)
		}
	}
	denied := []string{"notgo.dev", "go.dev.evil.example", "xgo.dev", "godev", "dev", ""}
	for _, host := range denied {
		if c.allows(host) {
			t.Errorf("%q was allowed by go.dev", host)
		}
	}
}

// An allowed host that resolves somewhere blocked is still refused. This is the
// shape of the attack: the attacker controls the DNS record, not the allowlist.
func TestAllowedHostResolvingToLoopbackIsRefused(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example": {"127.0.0.1"},
	})
	_, err := c.Fetch(context.Background(), "https://docs.example/x")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
}

// A name answering with one public and one blocked address is refused whole.
// Picking the public one would be cooperating with the trick.
func TestSplitResolutionIsRefusedEntirely(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example": {"93.184.216.34", "169.254.169.254"},
	})
	_, err := c.Fetch(context.Background(), "https://docs.example/x")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want the whole name refused", err)
	}
}

// Plaintext is refused rather than upgraded. A network position that can
// rewrite an http response is a position that can write the model's
// instructions.
func TestOnlyHTTPS(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example": {"93.184.216.34"},
	})
	for _, raw := range []string{
		"http://docs.example/x",
		"file:///etc/passwd",
		"ftp://docs.example/x",
		"gopher://docs.example/x",
		"data:text/html,<b>hi</b>",
	} {
		if _, err := c.Fetch(context.Background(), raw); !errors.Is(err, ErrScheme) {
			t.Errorf("Fetch(%q) = %v, want ErrScheme", raw, err)
		}
	}
}

// The allowlist names hosts, not endpoints. An arbitrary port on an allowed
// host turns "read their docs" into "reach anything they run".
func TestNonDefaultPortsAreRefused(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example": {"93.184.216.34"},
	})
	if _, err := c.Fetch(context.Background(), "https://docs.example:8443/x"); err == nil ||
		!strings.Contains(err.Error(), "port") {
		t.Fatalf("err = %v, want a refusal naming the port", err)
	}
	// The default port spelled out is the same endpoint and must work.
	if err := c.vetURL(context.Background(), mustURL(t, "https://docs.example:443/x")); err != nil {
		t.Fatalf("the explicit default port was refused: %v", err)
	}
}

// Credentials in a URL are either a leak or a lure, and this path never needs
// one.
func TestURLCredentialsAreRefused(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example": {"93.184.216.34"},
	})
	_, err := c.Fetch(context.Background(), "https://user:pass@docs.example/x")
	if err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("err = %v, want a refusal naming the credentials", err)
	}
}

// A literal IP is not a name and must not be handed to a resolver. It is also
// still subject to every address rule.
func TestLiteralAddressesAreCheckedNotResolved(t *testing.T) {
	var resolved atomic.Int32
	c := New([]string{"127.0.0.1"}, Limits{})
	c.resolve = func(ctx context.Context, host string) ([]netip.Addr, error) {
		resolved.Add(1)
		return systemResolve(ctx, host)
	}
	_, err := c.Fetch(context.Background(), "https://127.0.0.1/x")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress — allowlisting a literal must not "+
			"override the address rules", err)
	}
}

// The rebinding defence. vetURL saw a public address; the dialer must check
// again, because the second lookup is where the attack puts its answer.
func TestDialerRechecksTheAddress(t *testing.T) {
	var calls atomic.Int32
	c := New([]string{"docs.example"}, Limits{})
	c.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
		// First answer public, every answer after that loopback: the shape of
		// a rebinding record with a one-second TTL.
		if calls.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	}

	// The real sequence: the pre-flight check resolves once and is satisfied,
	// and the dial that follows resolves again. Calling dial alone would
	// consume the first answer and test nothing.
	if err := c.vetURL(context.Background(), mustURL(t, "https://docs.example/x")); err != nil {
		t.Fatalf("the pre-flight check rejected a public address: %v", err)
	}
	_, err := c.dial(context.Background(), "tcp", "docs.example:443")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("the dialer connected to a rebound address: %v", err)
	}
	if calls.Load() < 2 {
		t.Fatalf("the name was resolved %d time(s); the dialer reused the vetted answer "+
			"instead of checking again, which is the hole rebinding aims at", calls.Load())
	}
}

// The dialer applies the address rules on the real dial path, against a
// listener that actually exists — so this cannot pass because the connection
// happened to fail.
func TestDialerRefusesABlockedAddressItCouldHaveReached(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	addrPort := netip.MustParseAddrPort(listener.Addr().String())
	c := New([]string{"docs.example"}, Limits{})
	// Loopback is blocked, so this asserts the check fires on the real dial
	// path rather than asserting a successful connection to a blocked address.
	c.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{addrPort.Addr()}, nil
	}
	_, err = c.dial(context.Background(), "tcp",
		net.JoinHostPort("docs.example", fmt.Sprint(addrPort.Port())))
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("dial to a loopback listener = %v, want it refused", err)
	}
}

// A redirect is re-checked in full: a host that passed once has no standing to
// send this somewhere else.
func TestRedirectsAreRecheckedAndBounded(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example": {"93.184.216.34"},
		"evil.example": {"93.184.216.34"},
	})

	// Off the allowlist.
	err := c.checkRedirect(requestTo(t, "https://evil.example/x"), nil)
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a redirect off the allowlist = %v, want ErrNotAllowed", err)
	}
	// Downgraded to plaintext.
	if err := c.checkRedirect(requestTo(t, "http://docs.example/x"), nil); !errors.Is(err, ErrScheme) {
		t.Fatalf("a redirect to http = %v, want ErrScheme", err)
	}
	// Chain length.
	via := make([]*http.Request, c.limits.MaxRedirects)
	if err := c.checkRedirect(requestTo(t, "https://docs.example/y"), via); err == nil ||
		!strings.Contains(err.Error(), "redirect") {
		t.Fatalf("an over-long chain = %v, want a bound", err)
	}
}

// requestTo builds the request the redirect check is handed.
func requestTo(t *testing.T, raw string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// Limits are never zero. An unbounded fetch at the end of a tool call is a turn
// that can hang on someone else's server.
func TestLimitsAreAlwaysBounded(t *testing.T) {
	for _, in := range []Limits{{}, {Timeout: -1, MaxBytes: -1, MaxRedirects: -1}} {
		got := normalize(in)
		if got.Timeout <= 0 || got.MaxBytes <= 0 || got.MaxRedirects <= 0 {
			t.Fatalf("normalize(%+v) = %+v, which leaves something unbounded", in, got)
		}
		if got.Timeout > time.Minute {
			t.Fatalf("default timeout is %s, long enough to hold a turn open", got.Timeout)
		}
	}
}

// Content types this cannot read are refused rather than reduced to mojibake.
func TestOnlyTextualContentTypes(t *testing.T) {
	for _, ok := range []string{
		"text/html", "text/html; charset=utf-8", "TEXT/HTML",
		"text/plain", "text/markdown", "application/xhtml+xml", "application/json", "",
	} {
		if !textual(ok) {
			t.Errorf("textual(%q) = false", ok)
		}
	}
	for _, bad := range []string{
		"image/png", "application/pdf", "application/octet-stream",
		"video/mp4", "application/zip", "font/woff2",
	} {
		if textual(bad) {
			t.Errorf("textual(%q) = true", bad)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The test seams are package-private and default to the real checks. This
// asserts the defaults, because a seam that silently defaulted open would be a
// far worse bug than the one it exists to work around.
func TestAClientFromNewCarriesTheRealChecks(t *testing.T) {
	c := New([]string{"docs.example"}, Limits{})
	if reason, blocked := c.blocked(netip.MustParseAddr("169.254.169.254")); !blocked {
		t.Fatalf("a client from New does not block the metadata endpoint (reason %q)", reason)
	}
	if reason, blocked := c.blocked(netip.MustParseAddr("127.0.0.1")); !blocked {
		t.Fatalf("a client from New does not block loopback (reason %q)", reason)
	}
	// And the resolver is the system one, not a fixture left behind by a test.
	addrs, err := c.resolve(context.Background(), "93.184.216.34")
	if err != nil || len(addrs) != 1 || addrs[0].String() != "93.184.216.34" {
		t.Fatalf("resolve of a literal = %v, %v; want the address itself", addrs, err)
	}
}
