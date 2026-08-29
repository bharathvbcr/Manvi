// Package fetch is this harness's only outbound HTTP path.
//
// It exists because the system prompt used to tell every run to "verify current
// documentation and online references" while nothing here could fetch anything:
// web access was whatever an MCP server happened to provide, and a shell `curl`
// without a task lease is refused by name. An instruction whose only available
// form of compliance is to recall something and present it as checked is worse
// than no instruction.
//
// # Why in-process rather than an MCP server or a sidecar
//
// The reference MCP fetch server would have been less code here and was
// rejected on one property: this harness can gate the *call* to an MCP tool and
// has no say in where that server then goes. Its own README warns it "can
// access local/internal IP addresses". The same is true of any out-of-process
// fetcher — a Rust sidecar, a container, a hosted crawler. An egress policy is
// only enforceable where the socket is opened, so the socket is opened here.
//
// # What it refuses, and why each refusal is not optional
//
// The address space, first. A URL is chosen by a model, and a model's choice is
// downstream of whatever it just read — a README, a web page, a tool result.
// So the URL is untrusted input, and the classic result of trusting it is a
// request to 169.254.169.254 or to a service on localhost that answers to
// anything that can reach it. Loopback, private, link-local, unique-local,
// multicast and unspecified addresses are all refused, on every hop.
//
// DNS rebinding, second. Resolving a name, checking the addresses, and then
// handing the *name* to the transport is the standard hole: the second
// resolution is a different answer. So the vetted addresses are pinned into the
// dialer and the transport never resolves anything itself.
//
// Redirects, third. A host that passes every check is free to redirect to one
// that does not, so the whole check runs again on each hop, and the hops are
// bounded.
//
// Plaintext, fourth. HTTP is refused outright rather than upgraded. Anything on
// the wire ends up in a model's context, and a network position that can rewrite
// a plaintext response is a network position that can write the model's
// instructions.
//
// And the allowlist over all of it: an operator names the hosts this harness may
// reach, out of band, and an empty list means no fetching at all. Deny by
// default, because the failure of an allowlist that defaults to open is silent.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Limits bound one fetch. Zero values are not "unbounded" anywhere: New fills
// them with the defaults below, because an unbounded fetch at the end of a
// model's tool call is a turn that can hang on someone else's server.
type Limits struct {
	// Timeout bounds the whole request, connection through last byte.
	Timeout time.Duration
	// MaxBytes bounds what is read from the body. The reader stops there and
	// the result says it was cut; it is never presented as a whole page.
	MaxBytes int64
	// MaxRedirects bounds the hop count. Each hop is re-checked in full.
	MaxRedirects int
}

// The defaults, chosen for the one job this has: read a documentation page.
const (
	// defaultTimeout is generous for a page and far short of a stuck socket.
	defaultTimeout = 20 * time.Second
	// defaultMaxBytes is about a large documentation page. Past this the
	// content is being read for something other than its prose.
	defaultMaxBytes = 2 << 20
	// defaultMaxRedirects covers the ordinary http→canonical-host→trailing-slash
	// chain with room to spare, and stops a redirect loop being someone else's
	// decision about how long this runs.
	defaultMaxRedirects = 5
)

// Errors callers branch on. They are distinguished because they mean different
// things to an operator: one is a policy decision, the others are the shape of
// the request.
var (
	// ErrNotAllowed is a host the operator did not name.
	ErrNotAllowed = errors.New("fetch: host is not in the allowlist")
	// ErrBlockedAddress is a host that resolves somewhere this must not reach.
	ErrBlockedAddress = errors.New("fetch: host resolves to a blocked address")
	// ErrScheme is anything but https.
	ErrScheme = errors.New("fetch: only https is allowed")
	// ErrDisabled is the zero-allowlist case: fetching is off.
	ErrDisabled = errors.New("fetch: no host allowlist is configured")
)

// Client fetches documents under a policy.
type Client struct {
	allowed []string
	limits  Limits
	http    *http.Client
	// resolve is the name lookup, replaceable in tests. Production is the
	// system resolver; a test cannot be allowed to depend on one.
	resolve func(ctx context.Context, host string) ([]netip.Addr, error)
	// blocked is the address rule, replaceable in tests for the same reason.
	//
	// Unexported and set only by New, so there is no production path that can
	// weaken it: a caller outside this package cannot reach the field, and the
	// only value New ever assigns is the real check. The round-trip tests
	// serve from loopback, which the real check refuses by design, and they
	// need somewhere to say "this one address is the server under test"
	// without that concession existing anywhere a real run could take it.
	blocked func(netip.Addr) (string, bool)
}

// New builds a client for exactly these hosts.
//
// An empty or all-blank allowlist yields a client that refuses everything with
// ErrDisabled. That is the intended off switch, and it is the default: a
// harness whose operator has said nothing about network access does not have
// network access.
func New(allowed []string, limits Limits) *Client {
	c := &Client{limits: normalize(limits)}
	for _, host := range allowed {
		host = strings.ToLower(strings.TrimSpace(host))
		if host != "" {
			c.allowed = append(c.allowed, host)
		}
	}
	c.resolve = systemResolve
	c.blocked = blockedAddr
	c.http = &http.Client{
		Timeout: c.limits.Timeout,
		// The dialer only ever connects to an address this client already
		// vetted, and the transport is given no chance to resolve anything for
		// itself. That is what closes rebinding: without it, the check and the
		// connection ask DNS two different times and can get two different
		// answers.
		Transport: &http.Transport{
			DialContext:           c.dial,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			// No proxy. A proxy from the environment would send the request
			// somewhere this never vetted, which is the whole check walked
			// around by an environment variable.
			Proxy: nil,
		},
		CheckRedirect: c.checkRedirect,
	}
	return c
}

func normalize(l Limits) Limits {
	if l.Timeout <= 0 {
		l.Timeout = defaultTimeout
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultMaxBytes
	}
	if l.MaxRedirects <= 0 {
		l.MaxRedirects = defaultMaxRedirects
	}
	return l
}

// Enabled reports whether any host was allowed. It is the honest answer to
// "can this harness look something up", and the prompt asks it before telling a
// model to check the documentation.
func (c *Client) Enabled() bool { return c != nil && len(c.allowed) > 0 }

// Hosts returns the allowlist, for reporting.
func (c *Client) Hosts() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.allowed...)
}

// Document is one fetched page, already reduced to text.
type Document struct {
	// URL is the final URL after redirects, which is not always the one asked
	// for and is the one the content actually came from.
	URL string
	// Title is the document title when the markup carried one.
	Title string
	// Text is the extracted prose.
	Text string
	// Truncated is true when the body hit the byte cap. The text is a prefix of
	// the page, never the page.
	Truncated bool
	// Bytes is how much of the body was read.
	Bytes int64
	// ContentType is what the server said it sent.
	ContentType string
}

// Fetch retrieves and extracts one document.
func (c *Client) Fetch(ctx context.Context, raw string) (*Document, error) {
	if !c.Enabled() {
		return nil, ErrDisabled
	}
	target, err := c.vet(ctx, raw)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	// Named honestly. A harness that disguises itself as a browser is a harness
	// whose traffic an operator cannot identify in their own logs.
	req.Header.Set("User-Agent", "MANVI-harness/1 (+documentation lookup)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.1")
	// No cookies, no auth, no referer. This path exists to read public
	// documentation; anything that needs a credential is not its job, and
	// carrying one would make it a way to exfiltrate one.

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetch: %s returned %s", target.Host, resp.Status)
	}

	ctype := resp.Header.Get("Content-Type")
	if !textual(ctype) {
		// Refused rather than read. A binary body reduced to "text" is a wall
		// of mojibake that costs a model its context and tells it nothing.
		return nil, fmt.Errorf("fetch: %s returned %q, which is not a text document",
			target.Host, firstToken(ctype))
	}

	// One more than the cap, so hitting it is distinguishable from a body that
	// happens to be exactly the cap.
	limited := io.LimitReader(resp.Body, c.limits.MaxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("fetch: reading %s: %w", target.Host, err)
	}
	doc := &Document{
		URL:         resp.Request.URL.String(),
		Bytes:       int64(len(body)),
		ContentType: firstToken(ctype),
	}
	if int64(len(body)) > c.limits.MaxBytes {
		body = body[:c.limits.MaxBytes]
		doc.Bytes = c.limits.MaxBytes
		doc.Truncated = true
	}

	doc.Title, doc.Text = Extract(string(body), doc.ContentType)
	return doc, nil
}

// vet applies every rule that can be decided before a socket is opened.
func (c *Client) vet(ctx context.Context, raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("fetch: %q is not a URL: %w", raw, err)
	}
	if err := c.vetURL(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

// vetURL is the check, factored out because a redirect has to run all of it
// again. A host that passed once has no standing to send this somewhere else.
func (c *Client) vetURL(ctx context.Context, u *url.URL) error {
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: %q", ErrScheme, u.Scheme)
	}
	if u.User != nil {
		// Credentials in a URL are either a leak or a lure, and this path never
		// needs one.
		return fmt.Errorf("fetch: %q carries credentials in the URL", u.Host)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return fmt.Errorf("fetch: %q names no host", u.String())
	}
	if port := u.Port(); port != "" && port != "443" {
		// The allowlist names hosts, not endpoints. Allowing an arbitrary port
		// on an allowed host turns "you may read their docs" into "you may
		// reach anything they run".
		return fmt.Errorf("fetch: %q asks for port %s; only the default https port is allowed",
			host, port)
	}
	if !c.allows(host) {
		return fmt.Errorf("%w: %q (allowed: %s)", ErrNotAllowed, host, strings.Join(c.allowed, ", "))
	}
	addrs, err := c.resolve(ctx, host)
	if err != nil {
		return fmt.Errorf("fetch: %q did not resolve: %w", host, err)
	}
	if len(addrs) == 0 {
		return fmt.Errorf("fetch: %q resolved to no addresses", host)
	}
	for _, addr := range addrs {
		if reason, blocked := c.blocked(addr); blocked {
			// The whole name is refused, not the offending address alone. A
			// name that answers with one public and one loopback address is a
			// name being used to walk around this check, and picking the
			// public one would be cooperating with it.
			return fmt.Errorf("%w: %q resolves to %s (%s)", ErrBlockedAddress, host, addr, reason)
		}
	}
	return nil
}

// allows matches a host against the allowlist.
//
// An entry matches the host itself and its subdomains, so naming "go.dev"
// admits "pkg.go.dev". Matched on whole labels: "notgo.dev" does not match
// "go.dev", which a suffix comparison on the raw string would have allowed.
func (c *Client) allows(host string) bool {
	for _, entry := range c.allowed {
		if host == entry {
			return true
		}
		if strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// checkRedirect re-runs the full check on every hop and bounds the chain.
func (c *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= c.limits.MaxRedirects {
		return fmt.Errorf("fetch: more than %d redirects", c.limits.MaxRedirects)
	}
	return c.vetURL(req.Context(), req.URL)
}

// dial connects only to an address this client vetted.
//
// The transport hands it "host:port"; it resolves the host itself and re-checks
// every address, because between vetURL and here is exactly the window a
// rebinding attack aims at. Two lookups is the cost of not trusting the first
// one to still be true.
func (c *Client) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("fetch: %q is not a dialable address: %w", address, err)
	}
	addrs, err := c.resolve(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("fetch: %q did not resolve: %w", host, err)
	}
	var dialer net.Dialer
	dialer.Timeout = 10 * time.Second
	var lastErr error
	for _, addr := range addrs {
		if reason, blocked := c.blocked(addr); blocked {
			return nil, fmt.Errorf("%w: %q resolves to %s (%s)", ErrBlockedAddress, host, addr, reason)
		}
		// The literal address, never the name. Handing the name back to the
		// dialer would be a third lookup and a third chance to get a different
		// answer.
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no addresses")
	}
	return nil, fmt.Errorf("fetch: connecting to %q: %w", host, lastErr)
}

// blockedAddr reports whether an address is one this harness must not reach,
// and why.
//
// The reason travels with the verdict because these are the refusals most
// likely to look like a bug to whoever hits them: a developer pointing this at
// their own docs server on localhost deserves to be told that is the rule
// rather than left to read a connection error.
func blockedAddr(addr netip.Addr) (string, bool) {
	addr = addr.Unmap()
	switch {
	case !addr.IsValid():
		return "not a valid address", true
	case addr.IsUnspecified():
		return "the unspecified address", true
	case addr.IsLoopback():
		return "loopback", true
	case addr.IsPrivate():
		// Covers RFC1918 for v4 and unique-local (fc00::/7) for v6.
		return "a private network", true
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		// 169.254.0.0/16 is where cloud instance metadata lives, and it is the
		// single most valuable target a server-side request forgery has.
		return "link-local — this range holds cloud instance metadata", true
	case addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return "multicast", true
	}
	// Ranges the standard library has no predicate for, checked explicitly
	// rather than assumed public. Each is routable-looking and reaches
	// somewhere this has no business being.
	for _, blocked := range reservedPrefixes {
		if blocked.prefix.Contains(addr) {
			return blocked.reason, true
		}
	}
	return "", false
}

// reservedPrefixes are the ranges netip has no predicate for.
var reservedPrefixes = []struct {
	prefix netip.Prefix
	reason string
}{
	{netip.MustParsePrefix("100.64.0.0/10"), "carrier-grade NAT"},
	{netip.MustParsePrefix("192.0.0.0/24"), "IETF protocol assignments"},
	{netip.MustParsePrefix("192.0.2.0/24"), "documentation range"},
	{netip.MustParsePrefix("198.18.0.0/15"), "benchmarking range"},
	{netip.MustParsePrefix("198.51.100.0/24"), "documentation range"},
	{netip.MustParsePrefix("203.0.113.0/24"), "documentation range"},
	{netip.MustParsePrefix("240.0.0.0/4"), "reserved"},
	{netip.MustParsePrefix("::/128"), "the unspecified address"},
	{netip.MustParsePrefix("64:ff9b::/96"), "NAT64 — a translation of the v4 space"},
	{netip.MustParsePrefix("2001:db8::/32"), "documentation range"},
}

// systemResolve is the production lookup.
func systemResolve(ctx context.Context, host string) ([]netip.Addr, error) {
	// A literal address is not a name and must not be handed to a resolver,
	// which would either fail or, worse, succeed against something else.
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// textual reports whether a content type is prose this can extract.
func textual(ctype string) bool {
	switch firstToken(ctype) {
	case "text/html", "text/plain", "text/markdown", "text/x-markdown",
		"application/xhtml+xml", "application/json", "":
		// An empty type is accepted because plenty of documentation servers
		// send none, and the extractor treats unknown input as text anyway.
		return true
	}
	return false
}

// firstToken is the media type without its parameters, lowercased.
func firstToken(ctype string) string {
	if i := strings.IndexByte(ctype, ';'); i >= 0 {
		ctype = ctype[:i]
	}
	return strings.ToLower(strings.TrimSpace(ctype))
}
