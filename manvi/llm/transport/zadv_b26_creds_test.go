package transport

import (
	"net"
	"net/url"
	"strings"
	"testing"
)

// Adversarial pass on b26-creds: IsLoopbackURL spelling table.

// advB26ReachesLocalOnly reports whether Go's HTTP client, given this URL,
// dials a host that can only be this machine. It is the oracle a TRUE answer
// must agree with.
func advB26DialsLocalOnly(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if h == "localhost" || h == "0.0.0.0" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// TestAdvB26CredsIsLoopbackURLNeverTrueForARemoteHost: for hostile spellings a
// TRUE answer must mean the dialled host is this machine.
func TestAdvB26CredsIsLoopbackURLNeverTrueForARemoteHost(t *testing.T) {
	cases := []string{
		"http://localhost@evil.test:80/",
		"http://127.0.0.1@evil.test/",
		"http://127.0.0.1:80@evil.test/",
		"http://localhost:80%40evil.test/",
		"http://evil.test/?x=127.0.0.1",
		"http://evil.test/#localhost",
		"http://evil.test/localhost",
		"http://localhost.evil.test/",
		"http://127.0.0.1.evil.test/",
		"http://evil.test\\@localhost/",
		"http://evil.test\t@localhost/",
		"http://[::1%25eth0]:80/",
		"http://[::ffff:127.0.0.1]/",
		"http://[::ffff:8.8.8.8]/",
		"http://0.0.0.0/",
		"http://127.0.0.2/",
		"http://127.1/",
		"http://2130706433/",
		"http://0177.0.0.1/",
		"http://0x7f.0.0.1/",
		"http://LOCALHOST/",
		"http://LocalHost:8000/",
		"http://localhost./",
		"http://localhost:99999999999/",
		"http://%6c%6f%63%61%6c%68%6f%73%74/",
		"http://localhost%00.evil.test/",
		"http://\x7f/",
		"localhost:8000/v1",
		"127.0.0.1:8000/v1",
		"//localhost/v1",
		"http:///v1",
		"",
		"http://127.0.0.1:8000/v1\n@evil.test",
	}
	for _, raw := range cases {
		got := IsLoopbackURL(raw)
		t.Logf("IsLoopbackURL(%q) = %v", raw, got)
		if got && !advB26DialsLocalOnly(raw) {
			t.Errorf("IsLoopbackURL(%q) = true but the dialled host is not this machine", raw)
		}
	}
}

// TestAdvB26CredsIsLoopbackURLUnparseableIsFalse: the parse-error fallback
// must be closed even when a loopback literal appears anywhere in the string.
func TestAdvB26CredsIsLoopbackURLUnparseableIsFalse(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8000/\x7f",
		"http://localhost/%zz",
		"http://[::1/",
		"http://127.0.0.1:abc/",
		"http://evil.test/%zz?u=127.0.0.1",
		"://localhost",
		"127.0.0.1:8000/v1",
	} {
		if _, err := url.Parse(raw); err == nil {
			continue
		}
		if IsLoopbackURL(raw) {
			t.Errorf("an unparseable URL %q was answered loopback", raw)
		}
	}
}

// TestAdvB26CredsIsLoopbackURLTrueSpellings: the answers a loopback operator
// needs to stay true.
func TestAdvB26CredsIsLoopbackURLTrueSpellings(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:8000/v1", "http://localhost:8000/v1", "http://[::1]:8000/v1",
		"https://127.0.0.1/v1", "http://127.0.0.2/v1", "http://[::ffff:127.0.0.1]:80/v1",
	} {
		if !IsLoopbackURL(raw) {
			t.Errorf("IsLoopbackURL(%q) = false", raw)
		}
	}
}

// TestAdvB26CredsIsLoopbackURLHostnamesAreCaseInsensitive: RFC 4343 hostnames
// are case-insensitive, so an operator who typed LOCALHOST named this machine.
// The failure direction is closed (a borrowed key is refused), so this is a
// usability defect, not a leak.
func TestAdvB26CredsIsLoopbackURLHostnamesAreCaseInsensitive(t *testing.T) {
	for _, raw := range []string{"http://LOCALHOST:8000/v1", "http://LocalHost/v1"} {
		if !IsLoopbackURL(raw) {
			t.Errorf("IsLoopbackURL(%q) = false: a loopback hostname spelled in capitals is refused", raw)
		}
	}
}
