package fetch

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
)

// One address in every range the policy must refuse, including the ones that
// only look public because they are a translation or tunnel of something else.
func TestBlockedAddrCoversTranslationAndReservedRanges(t *testing.T) {
	cases := map[string]string{
		// 0.0.0.0/8 — "this network"; Linux routes 0.x.y.z to the local host.
		"0.0.0.1":         "0.0.0.0/8",
		"0.1.2.3":         "0.0.0.0/8",
		"0.255.255.255":   "0.0.0.0/8",
		"::ffff:0.0.0.1":  "0.0.0.0/8 via v4-mapped",
		"255.255.255.255": "limited broadcast",
		"192.88.99.1":     "deprecated 6to4 relay anycast",
		// 6to4 — the embedded v4 is attacker-chosen, so the whole /16 goes.
		"2002::1":                  "6to4",
		"2002:7f00:1::1":           "6to4 embedding 127.0.0.1",
		"2002:a9fe:a9fe::1":        "6to4 embedding 169.254.169.254",
		"2002:ffff:ffff:ffff::1":   "6to4 top of range",
		"2002:0a00:0001:0:0:0:0:1": "6to4 embedding 10.0.0.1",
		// NAT64, well-known and local-use.
		"64:ff9b::1":             "NAT64 well-known",
		"64:ff9b::a9fe:a9fe":     "NAT64 embedding 169.254.169.254",
		"64:ff9b:1::1":           "NAT64 local-use",
		"64:ff9b:1:ffff::a9fe:1": "NAT64 local-use, top of range",
		"64:ff9b:1:0:0:0:7f00:1": "NAT64 local-use embedding loopback",
		// IPv4-compatible (deprecated) and mapped forms.
		"::7f00:1":         "v4-compatible loopback",
		"::a9fe:a9fe":      "v4-compatible metadata",
		"::0a00:1":         "v4-compatible 10.0.0.1",
		"::808:808":        "v4-compatible 8.8.8.8 — deprecated form, refused whole",
		"::ffff:10.0.0.1":  "v4-mapped private",
		"::ffff:a9fe:a9fe": "v4-mapped metadata, hex form",
		// Teredo embeds an obfuscated v4 and a server.
		"2001:0:4136:e378:8000:63bf:3fff:fdd2": "Teredo",
		"2001::1":                              "Teredo base",
		// Other v6 special-purpose space.
		"fec0::1":     "site-local",
		"feff::1":     "site-local top",
		"fc00::1":     "unique-local",
		"fdff::1":     "unique-local top",
		"fe80::1":     "link-local",
		"ff02::1":     "multicast",
		"100::1":      "discard-only",
		"2001:10::1":  "ORCHID",
		"2001:db8::1": "documentation",
		"3fff::1":     "documentation (RFC 9637)",
		"3fff:fff::1": "documentation top",
		"5f00::1":     "SRv6 SIDs",
		"2001:2::1":   "benchmarking",
		// v4 special-purpose space.
		"100.64.0.1":      "CGNAT",
		"100.127.255.254": "CGNAT top",
		"198.18.0.1":      "benchmarking",
		"198.19.255.255":  "benchmarking top",
		"240.0.0.1":       "reserved",
		"224.0.0.1":       "multicast",
		"239.255.255.255": "multicast top",
		"169.254.169.254": "link-local metadata",
		"192.0.0.9":       "IETF protocol assignments",
		"192.0.2.1":       "TEST-NET-1",
		"198.51.100.1":    "TEST-NET-2",
		"203.0.113.1":     "TEST-NET-3",
		"127.0.0.1":       "loopback",
		"10.0.0.1":        "private",
		"172.31.255.255":  "private",
		"192.168.0.1":     "private",
		"0.0.0.0":         "unspecified",
		"::":              "unspecified",
		"::1":             "loopback",
	}
	for addr, why := range cases {
		t.Run(addr, func(t *testing.T) {
			reason, blocked := blockedAddr(netip.MustParseAddr(addr))
			if !blocked {
				t.Fatalf("%s (%s) was not blocked", addr, why)
			}
			if reason == "" {
				t.Fatal("blocked without a reason")
			}
		})
	}
}

// The neighbours of each blocked range must stay open, or the table is a
// refusal engine rather than a boundary.
func TestBlockedAddrBoundariesStayOpen(t *testing.T) {
	for _, addr := range []string{
		"1.0.0.0", "93.184.216.34", "1.1.1.1", "8.8.8.8", "9.255.255.255", "11.0.0.1",
		"100.63.255.255", "100.128.0.0", "198.17.255.255", "198.20.0.0",
		"172.15.255.255", "172.32.0.0", "223.255.255.255", "192.88.98.1", "192.88.100.1",
		"2606:4700::1111", "2001:4860:4860::8888", "2a00:1450:4001::200e",
		"2001:200::1", "2003::1", "2001:db9::1", "64:ff9a::1", "64:ff9c::1",
		"64:ff9b:2::1", "3ffe::1", "4000::1", "fbff::1", "fe7f::1",
		"2620:fe::fe",
	} {
		if reason, blocked := blockedAddr(netip.MustParseAddr(addr)); blocked {
			t.Errorf("%s was blocked as %q", addr, reason)
		}
	}
}

// netip.Prefix.Contains is false for any address carrying a zone, so a zoned
// literal would walk past every prefix-list entry. The zone is not part of the
// address being judged.
func TestBlockedAddrIgnoresZones(t *testing.T) {
	for _, s := range []string{
		"2002:7f00:1::1%eth0", "64:ff9b:1::1%lo0", "fec0::1%1", "2001::1%en0",
		"::7f00:1%x", "fe80::1%eth0", "::1%lo",
	} {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Fatalf("ParseAddr(%q): %v", s, err)
		}
		if _, blocked := blockedAddr(addr); !blocked {
			t.Errorf("%s was not blocked", s)
		}
	}
	// A zone on a public address does not make it blocked either.
	if reason, blocked := blockedAddr(netip.MustParseAddr("2606:4700::1111%eth0")); blocked {
		t.Errorf("zoned public address blocked as %q", reason)
	}
}

// Forms a URL host can take that are not dotted-quad. url.Parse leaves the
// host as written; none of them may be treated as a public name, and none may
// reach the resolver as a literal that parses to something blocked.
func TestVetRefusesNonCanonicalIPLiterals(t *testing.T) {
	hosts := []string{
		"0x7f.1", "2130706433", "0177.0.0.1", "127.1", "0x7f000001",
		"017700000001", "0.0.0.0", "0", "[::1]", "[::ffff:127.0.0.1]",
		"[2002:7f00:1::1]", "[2002:7F00:1::1]", "[FE80::1%25eth0]", "[2001:DB8::1]",
		"[64:ff9b:1::1]", "[::7f00:1]", "127.0.0.1.", "127.0.0.1",
	}
	for _, h := range hosts {
		t.Run(h, func(t *testing.T) {
			u := mustURL(t, "https://"+h+"/x")
			// The allowlist names the host exactly as parsed, the worst case: the
			// operator's gate is open and only the address rules are left.
			c := New([]string{u.Hostname()}, Limits{})
			var lookups []string
			c.resolve = func(_ context.Context, host string) ([]netip.Addr, error) {
				if a, err := netip.ParseAddr(host); err == nil {
					return []netip.Addr{a}, nil
				}
				// A resolver that does not understand the odd forms and says
				// "public". No real lookup is made.
				lookups = append(lookups, host)
				return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
			}
			if err := c.vetURL(context.Background(), u); err == nil {
				t.Fatalf("https://%s/ was accepted", h)
			}
			if len(lookups) > 0 {
				t.Fatalf("numeric-looking host %q reached the resolver", h)
			}
		})
	}
}

// A trailing dot or upper case must not step around the allowlist or the
// address rule.
func TestVetNormalisesTheHost(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example":  {"93.184.216.34"},
		"docs.example.": {"127.0.0.1"},
	})
	if err := c.vetURL(context.Background(), mustURL(t, "https://DOCS.example/x")); err != nil {
		t.Fatalf("upper-case host rejected: %v", err)
	}
	// A rooted name is a different string to the allowlist; it must be refused
	// rather than resolved on the strength of a suffix match.
	if err := c.vetURL(context.Background(), mustURL(t, "https://docs.example./x")); err == nil {
		t.Fatal("a trailing-dot host was accepted")
	}
}

// Rebinding at the dialer: a public answer first, a blocked one on every later
// lookup, for each newly covered range, not only loopback.
func TestDialerRefusesReboundNewRanges(t *testing.T) {
	for _, bound := range []string{"0.0.0.5", "2002:7f00:1::1", "64:ff9b:1::7f00:1", "::7f00:1", "2001::1"} {
		t.Run(bound, func(t *testing.T) {
			var calls atomic.Int32
			c := New([]string{"docs.example"}, Limits{})
			c.resolve = func(context.Context, string) ([]netip.Addr, error) {
				if calls.Add(1) == 1 {
					return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
				}
				return []netip.Addr{netip.MustParseAddr("93.184.216.34"), netip.MustParseAddr(bound)}, nil
			}
			if err := c.vetURL(context.Background(), mustURL(t, "https://docs.example/x")); err != nil {
				t.Fatalf("pre-flight: %v", err)
			}
			_, err := c.dial(context.Background(), "tcp", "docs.example:443")
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("dial to rebound %s = %v, want ErrBlockedAddress", bound, err)
			}
		})
	}
}

// A redirect to a literal blocked address is refused even when the allowlist
// would otherwise match, and a redirect to an allowed name that now resolves
// to a blocked range is refused.
func TestRedirectToBlockedRangesIsRefused(t *testing.T) {
	c := withResolver(t, []string{"docs.example"}, map[string][]string{
		"docs.example":   {"93.184.216.34"},
		"a.docs.example": {"2002:a9fe:a9fe::1"},
		"b.docs.example": {"0.0.0.1"},
	})
	for _, target := range []string{
		"https://a.docs.example/", "https://b.docs.example/",
		"https://169.254.169.254/", "https://[2002:a9fe:a9fe::1]/",
	} {
		req := &http.Request{URL: mustURL(t, target)}
		if err := c.checkRedirect(req, nil); err == nil {
			t.Errorf("redirect to %s was followed", target)
		}
	}
}

// Fuzz: the classifier never panics, and its verdict equals membership in the
// documented blocked set after unwrapping (v4-mapped, zone stripped).
func FuzzBlockedAddr(f *testing.F) {
	for _, s := range []string{"0.0.0.1", "2002::1", "64:ff9b:1::1", "::ffff:1.2.3.4", "8.8.8.8", "2606:4700::1111", "fe80::1%x", "::"} {
		f.Add(s)
	}
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			addr = netip.Addr{}
		}
		reason, blocked := blockedAddr(addr) // must not panic
		if blocked && reason == "" {
			t.Fatalf("%q blocked without a reason", s)
		}
		u := addr.Unmap().WithZone("")
		want := !u.IsValid() || u.IsUnspecified() || u.IsLoopback() || u.IsPrivate() ||
			u.IsLinkLocalUnicast() || u.IsLinkLocalMulticast() || u.IsMulticast()
		for _, p := range reservedPrefixes {
			if p.prefix.Contains(u) {
				want = true
			}
		}
		if blocked != want {
			t.Fatalf("blockedAddr(%q) = %v, want %v", s, blocked, want)
		}
		// Every address the list names as embedding a v4 must be judged as
		// blocked regardless of which v4 it embeds.
		if u.Is6() && !u.Is4In6() {
			b := u.As16()
			if b[0] == 0x20 && b[1] == 0x02 && !blocked {
				t.Fatalf("6to4 address %q allowed", s)
			}
		}
		if u.Is4() && u.As4()[0] == 0 && !blocked {
			t.Fatalf("0/8 address %q allowed", s)
		}
	})
}
