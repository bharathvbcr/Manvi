package fetch

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Adversarial probes for ft-e58f537b. None touches the external network: every
// resolver is a stub, and anything that would dial runs under a cancelled or
// short context against an address the policy must refuse first.

// An ambient proxy must never receive the request. Transport.Proxy is nil, so
// the dialer must be asked for the target host and never for the proxy.
func TestAdvProxyEnvironmentIsIgnored(t *testing.T) {
	proxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	var hits atomic.Int32
	go func() {
		for {
			conn, err := proxy.Accept()
			if err != nil {
				return
			}
			hits.Add(1)
			conn.Close()
		}
	}()
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY"} {
		t.Setenv(k, "http://"+proxy.Addr().String())
	}
	t.Setenv("NO_PROXY", "")

	c := New([]string{"docs.example"}, Limits{Timeout: 2 * time.Second})
	if tr := c.http.Transport.(*http.Transport); tr.Proxy != nil {
		t.Fatal("Transport.Proxy is set")
	}
	var dialed []string
	var mu sync.Mutex
	c.resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
	}
	c.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		return nil, errors.New("stop")
	}
	_, _ = c.Fetch(context.Background(), "https://docs.example/x")
	mu.Lock()
	defer mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "docs.example:443" {
		t.Fatalf("dialer was asked for %v, want exactly docs.example:443", dialed)
	}
	if hits.Load() != 0 {
		t.Fatalf("proxy contacted %d times", hits.Load())
	}
}

// resolverFunc builds a client whose resolver is the given map and whose
// transport uses the REAL dial path, so a refused address never opens a socket.
func advClient(t *testing.T, allowed []string, resolve func(host string) ([]netip.Addr, error)) *Client {
	t.Helper()
	c := New(allowed, Limits{Timeout: 3 * time.Second})
	c.resolve = func(_ context.Context, host string) ([]netip.Addr, error) { return resolve(host) }
	return c
}

// A name that is public on the first hop and private on the redirect hop.
func TestAdvRedirectToHostPrivateOnlyOnRedirectHop(t *testing.T) {
	var secondHopRequests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHopRequests.Add(1)
		http.Redirect(w, r, "https://internal.docs.example/secret", http.StatusFound)
	}))
	defer srv.Close()
	srvAddr := netip.MustParseAddrPort(strings.TrimPrefix(srv.URL, "https://"))

	c := advClient(t, []string{"docs.example"}, func(host string) ([]netip.Addr, error) {
		if host == "internal.docs.example" {
			return []netip.Addr{netip.MustParseAddr("10.1.2.3")}, nil
		}
		return []netip.Addr{srvAddr.Addr()}, nil
	})
	real := blockedAddr
	c.blocked = func(a netip.Addr) (string, bool) {
		if a == srvAddr.Addr() {
			return "", false
		}
		return real(a)
	}
	tr := c.http.Transport.(*http.Transport)
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "internal.") {
			t.Errorf("dialled the redirect target %q", addr)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, srvAddr.String())
	}
	tr.TLSClientConfig = insecureTLS()
	_, err := c.Fetch(context.Background(), "https://docs.example/start")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress", err)
	}
	if n := secondHopRequests.Load(); n != 1 {
		t.Fatalf("server saw %d requests, want 1", n)
	}
}

// Redirects to other schemes, ports, credentials, IP literals, protocol-relative.
func TestAdvRedirectTargetsAreAllRefused(t *testing.T) {
	for _, loc := range []string{
		"http://docs.example/", "https://docs.example:8443/", "https://u:p@docs.example/",
		"https://127.0.0.1/", "https://[::1]/", "//127.0.0.1/", "https://evil.example/",
		"ftp://docs.example/", "file:///etc/passwd", "https://docs.example.evil.example/",
		"https://169.254.169.254/latest/meta-data/", "https://2130706433/", "https://0x7f.1/",
	} {
		t.Run(loc, func(t *testing.T) {
			c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
			})
			tr := c.http.Transport.(*http.Transport)
			var dials atomic.Int32
			tr.DialContext = func(context.Context, string, string) (net.Conn, error) {
				dials.Add(1)
				a, b := net.Pipe()
				go func() { _ = b.Close() }()
				return a, nil
			}
			req, _ := http.NewRequest(http.MethodGet, "https://docs.example/", nil)
			next, _ := http.NewRequest(http.MethodGet, loc, nil)
			if strings.HasPrefix(loc, "//") {
				next.URL.Host = strings.TrimPrefix(strings.SplitN(loc, "/", 4)[2], "")
				next.URL.Scheme = "https"
			}
			if err := c.checkRedirect(next, []*http.Request{req}); err == nil {
				t.Fatalf("redirect to %q was allowed", loc)
			}
		})
	}
}

// Resolver answers that spell a v4 address in v6: every form must be refused
// at vet time AND at dial time, using the production resolver for literals.
func TestAdvResolverV6SpellingsOfV4(t *testing.T) {
	for _, s := range []string{
		"::ffff:127.0.0.1", "::ffff:7f00:1", "::127.0.0.1", "::7f00:1", "64:ff9b::7f00:1",
		"64:ff9b::a9fe:a9fe", "64:ff9b:1::7f00:1", "2002:7f00:1::", "::ffff:169.254.169.254",
		"::ffff:10.0.0.1%eth0", "0:0:0:0:0:ffff:7f00:1",
	} {
		t.Run(s, func(t *testing.T) {
			addr := netip.MustParseAddr(s)
			c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) {
				return []netip.Addr{addr}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := c.dial(ctx, "tcp", "docs.example:443"); !errors.Is(err, ErrBlockedAddress) {
				t.Errorf("dial err = %v, want ErrBlockedAddress", err)
			}
			if err := c.vetURL(context.Background(), mustURL(t, "https://docs.example/")); !errors.Is(err, ErrBlockedAddress) {
				t.Errorf("vet err = %v, want ErrBlockedAddress", err)
			}
		})
	}
}

// RFC 2765 SIIT "IPv4-translated" space ::ffff:0:0:0/96 embeds a v4 address the
// same way the other ranges do and is NOT in reservedPrefixes. Expected to fail.
func TestAdvSIITTranslatedPrefixBlocked(t *testing.T) {
	for _, s := range []string{"::ffff:0:7f00:1", "::ffff:0:a9fe:a9fe", "0:0:0:0:ffff:0:a00:1"} {
		if reason, blocked := blockedAddr(netip.MustParseAddr(s)); !blocked {
			t.Errorf("%s (SIIT-translated, embeds a v4 address) is not blocked; reason=%q", s, reason)
		}
	}
}

// Rebinding: public at vet, loopback at dial. The dial must refuse and no
// socket may be opened.
func TestAdvRebindingBetweenVetAndDial(t *testing.T) {
	var calls atomic.Int32
	c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) {
		if calls.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	_, err := c.Fetch(context.Background(), "https://docs.example/")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v, want ErrBlockedAddress (calls=%d)", err, calls.Load())
	}
	if calls.Load() < 2 {
		t.Fatalf("resolver called %d times; dial did not re-resolve", calls.Load())
	}
}

// Mixed answers are refused whole and, with the context already cancelled, any
// dial attempt to the public member would surface as context.Canceled.
func TestAdvMixedAnswerNeverDialsPublicMember(t *testing.T) {
	for _, order := range [][]string{
		{"8.8.8.8", "127.0.0.1"}, {"127.0.0.1", "8.8.8.8"}, {"8.8.8.8", "1.1.1.1", "::ffff:10.0.0.1"},
		{"2606:4700::1111", "64:ff9b:1::1"}, {"8.8.8.8", "0.0.0.7"},
	} {
		var addrs []netip.Addr
		for _, s := range order {
			addrs = append(addrs, netip.MustParseAddr(s))
		}
		c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) { return addrs, nil })
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := c.dial(ctx, "tcp", "docs.example:443")
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%v: err = %v, want ErrBlockedAddress", order, err)
		}
	}
}

// Non-https schemes and URL confusion. Every one must be refused, and the
// resolver (answering loopback for everything) must make a hit impossible.
func TestAdvURLConfusion(t *testing.T) {
	for _, raw := range []string{
		"file:///etc/passwd", "gopher://docs.example/", "ftp://docs.example/", "data:text/html,hi",
		"http://docs.example/", "http+unix://%2Fvar%2Frun%2Fdocker.sock/", "unix:///var/run/docker.sock",
		"https:docs.example", "https:/docs.example", "//docs.example/",
		"https://public@127.0.0.1/", "https://docs.example@127.0.0.1/", "https://127.0.0.1#@docs.example/",
		"https://127.0.0.1?@docs.example/", "https://127.0.0.1/@docs.example", `https://docs.example\@127.0.0.1/`,
		`https://127.0.0.1\@docs.example/`, `https://127.0.0.1\.docs.example/`, "https://127.0.0.1%2f.docs.example/",
		"https://docs.example%2e127.0.0.1/", "https://%31%32%37.0.0.1.docs.example/", "https://127.0.0.1.",
		"https://１２７.０.０.１/", "https://127。0。0。1/", "https://①②⑦.0.0.1/", "https://0x7f000001/",
		"https://017700000001/", "https://127.1/", "https://[::ffff:127.0.0.1]/", "https://[::1]/",
		"https://[0:0:0:0:0:ffff:7f00:1]/", "https://localhost/", "https://docs.example:443@127.0.0.1/",
		"https://docs.example:/", "https:///docs.example", "HTTPS://127.0.0.1/", " https://127.0.0.1/",
		"https://docs.example\x00.evil/", "https://docs.example\n.evil/",
	} {
		t.Run(raw, func(t *testing.T) {
			c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) {
				return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // any real dial surfaces as a non-policy error
			_, err := c.Fetch(ctx, raw)
			if err == nil {
				t.Fatalf("%q fetched", raw)
			}
			if errors.Is(err, context.Canceled) && !errors.Is(err, ErrBlockedAddress) {
				// Reached the transport with a cancelled ctx: vet passed. That is
				// only acceptable if the host really is docs.example.
				t.Logf("%q passed vet and reached the transport", raw)
			}
		})
	}
}

// Same table, but through the production resolver with IP-literal allowlist
// entries (the only way a numeric host can pass the allowlist): the literal
// must still be refused by the address rule.
func TestAdvAllowlistedLiteralsStillBlocked(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "::ffff:127.0.0.1", "64:ff9b::7f00:1", "0.0.0.0", "169.254.169.254"} {
		c := New([]string{host}, Limits{Timeout: time.Second})
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		u := "https://" + host + "/"
		if strings.Contains(host, ":") {
			u = "https://[" + host + "]/"
		}
		_, err := c.Fetch(ctx, u)
		if !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s: err = %v, want ErrBlockedAddress", host, err)
		}
	}
}

// Resolver failure modes must fail closed.
func TestAdvResolverFailuresFailClosed(t *testing.T) {
	cases := map[string]func(context.Context, string) ([]netip.Addr, error){
		"error":        func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("SERVFAIL") },
		"empty":        func(context.Context, string) ([]netip.Addr, error) { return nil, nil },
		"invalid addr": func(context.Context, string) ([]netip.Addr, error) { return []netip.Addr{{}}, nil },
		"partial+err": func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("8.8.8.8")}, context.DeadlineExceeded
		},
		"blocks until ctx": func(ctx context.Context, _ string) ([]netip.Addr, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	for name, resolve := range cases {
		t.Run(name, func(t *testing.T) {
			c := New([]string{"docs.example"}, Limits{Timeout: 300 * time.Millisecond})
			c.resolve = resolve
			start := time.Now()
			// The test's own guard, so a missing bound shows up as a failure and
			// not a hang. Limits.Timeout is 300ms; the caller supplied no deadline.
			guard, stop := context.WithTimeout(context.Background(), 3*time.Second)
			defer stop()
			doc, err := c.Fetch(guard, "https://docs.example/")
			if err == nil || doc != nil {
				t.Fatalf("doc=%v err=%v; want a refusal", doc, err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("Fetch took %v with Limits.Timeout=300ms: the resolve in vet is not bounded by it", d)
			}
			if _, e := c.dial(cancelled(), "tcp", "docs.example:443"); e == nil {
				t.Fatal("dial succeeded")
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("took %v", d)
			}
		})
	}
}

// A huge answer list: the policy must stay linear and still find the one bad
// member at the very end.
func TestAdvHugeAnswerListWithBadTail(t *testing.T) {
	var addrs []netip.Addr
	for i := 0; i < 200000; i++ {
		addrs = append(addrs, netip.AddrFrom4([4]byte{8, byte(i >> 16), byte(i >> 8), byte(i)}))
	}
	addrs = append(addrs, netip.MustParseAddr("127.0.0.1"))
	c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) { return addrs, nil })
	start := time.Now()
	if _, err := c.dial(cancelled(), "tcp", "docs.example:443"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v", d)
	}
}

// A whole-public answer where every dial fails must stop promptly once the
// context ends rather than grind through the list at 10s apiece.
func TestAdvDialLoopHonoursContext(t *testing.T) {
	var addrs []netip.Addr
	for i := 0; i < 2000; i++ {
		addrs = append(addrs, netip.AddrFrom4([4]byte{8, 8, byte(i >> 8), byte(i)}))
	}
	c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) { return addrs, nil })
	start := time.Now()
	_, err := c.dial(cancelled(), "tcp", "docs.example:443")
	if err == nil {
		t.Fatal("dial succeeded")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("cancelled dial loop took %v", d)
	}
}

// Policy report only: which ports and hosts exist as rules.
func TestAdvPortPolicy(t *testing.T) {
	c := advClient(t, []string{"docs.example"}, func(string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	for _, p := range []string{"22", "25", "80", "8443", "0443", "00443", "+443", "443 "} {
		u := mustURL(t, "https://docs.example/")
		u.Host = "docs.example:" + p
		if err := c.vetURL(context.Background(), u); err == nil || errors.Is(err, ErrBlockedAddress) {
			t.Errorf("port %q: err = %v; want a port refusal before resolution", p, err)
		}
	}
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func insecureTLS() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
