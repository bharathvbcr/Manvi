// Package transport is the shared HTTP and SSE substrate every provider
// adapter sits on.
//
// It is hand-rolled rather than delegated to a vendor SDK, deliberately. Three
// adapters that share one transport share one retry policy, one error
// taxonomy, one cancellation story, and one set of tests — where three SDKs
// would each bring their own, and a bug in any of them would be reproducible
// only through that vendor's abstractions. It also keeps the execution plane at
// zero external dependencies, which is what keeps CGO_ENABLED=0 and the single
// static binary intact.
//
// The reliability rules it enforces, uniformly:
//
//   - Retries are bounded, backed off exponentially, and jittered. An unbounded
//     retry is an outage amplifier.
//   - Retry-After is obeyed when the server sends one. Guessing a delay when
//     the server has told you the answer is how a rate limit becomes a ban.
//   - Only genuinely transient failures retry. A 400 is a bug in the request
//     and retrying it wastes budget and hides the defect.
//   - Cancellation propagates. A cancelled parent must not leave a request
//     running, or fan-out cancellation is cosmetic.
//   - Every failure carries the provider, the status, and the response body.
//     "request failed" is not a diagnosis.
package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Error is a failed provider call.
type Error struct {
	Provider string
	// Status is the HTTP status, or 0 for a transport-level failure.
	Status int
	// Body is the response body, truncated. Kept because provider error
	// messages are the fastest route to the actual cause.
	Body string
	// RequestID is the provider's trace identifier when it supplies one.
	RequestID string
	// Attempts is how many times the call was tried.
	Attempts int
	// Err is the underlying transport error, if any.
	Err error

	// inStream marks a failure the wire reported inside a response the
	// transport had already accepted with a 2xx.
	inStream bool
	// retryHint is the raw Retry-After header. Unexported: it is transport
	// plumbing, not something a caller should branch on.
	retryHint string
	// cancelled records that this failure is the caller's own cancellation or
	// expired deadline rather than a fault of the provider. Unexported because
	// callers should ask Retryable() rather than reconstruct the reasoning.
	cancelled bool
	// permanent records that this failure was decided before the request left
	// the process, so no attempt can change it.
	//
	// It exists because Status 0 means "never reached a server", and the two
	// ways that happens are not alike: a refused dial is the most retryable
	// fault there is, while a credential that could not be resolved — or that
	// this harness declined to send — will resolve exactly the same way on
	// every attempt. Without the distinction, attempt()'s own comment ("a
	// credential that cannot be resolved is not a transient failure") was a
	// claim the classification did not honour: the failure was retried until
	// the turn's deadline and then reported as "context deadline exceeded",
	// which names the timer instead of the cause and sends an operator looking
	// for a network problem that is not there.
	permanent bool
	// notSent is true only when failure occurred before HTTP.Do was called.
	notSent bool
}

func (e *Error) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: ", e.Provider)
	if e.Status > 0 {
		fmt.Fprintf(&b, "http %d", e.Status)
	} else {
		fmt.Fprintf(&b, "transport failure")
	}
	if e.Attempts > 1 {
		fmt.Fprintf(&b, " after %d attempts", e.Attempts)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " (request %s)", e.RequestID)
	}
	if e.Body != "" {
		fmt.Fprintf(&b, ": %s", e.Body)
	}
	if e.Err != nil && e.Err.Error() != e.Body {
		// Not repeated when it is the body's own text, which is what
		// StreamFailure builds: the same sentence twice reads as two faults.
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether this failure could succeed on a retry.
//
// A transport-level failure (Status 0) is retryable because it is usually a
// dropped connection or a refused dial, both of which routinely succeed on the
// next attempt. The exception is the caller's own cancellation: the context is
// already done, so no attempt made under it can do anything but fail again, and
// classifying it as transient makes this taxonomy lie to everything that reads
// it — including a caller deciding whether to fail over to another provider.
func (e *Error) Retryable() bool {
	if e.cancelled || e.permanent {
		return false
	}
	return retryableStatus(e.Status) || e.Status == 0
}

// retryableStatus classifies a status code.
//
// 408 request timeout, 409 conflict, 429 rate limited, and 5xx are transient.
// 529 is Anthropic's overloaded signal and falls in the 5xx range already, but
// is named here because it is easy to mistake for a client error.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests:
		return true
	}
	return status >= 500
}

// RetryPolicy bounds how hard the transport tries.
type RetryPolicy struct {
	// MaxAttempts includes the first try. 1 disables retrying.
	MaxAttempts int
	// BaseDelay is the first backoff interval.
	BaseDelay time.Duration
	// MaxDelay caps exponential growth.
	MaxDelay time.Duration
	// Jitter is the fraction of the delay randomised, in [0,1]; a value outside
	// that range is held to it. Without it,
	// every client that failed together retries together. A backoff is spread
	// around its computed value; a server's Retry-After is only ever lengthened,
	// by up to this fraction of it.
	Jitter float64
	// MaxRetryAfter bounds how long a server-supplied Retry-After can park the
	// harness. It is an absolute ceiling rather than a multiple of MaxDelay:
	// the two answer different questions, and deriving one from the other makes
	// a short backoff silently shorten the limit on how long we will honour a
	// server's own instruction. Zero means the default. It bounds what the
	// server may ask for; the jitter added to that is on top of it.
	MaxRetryAfter time.Duration
}

// DefaultMaxRetryAfter is the ceiling applied when a policy does not set one.
// Long enough to ride out a real rate-limit window, short enough that a buggy
// or hostile header cannot strand a turn.
const DefaultMaxRetryAfter = 60 * time.Second

// DefaultRetryPolicy is deliberately conservative: four attempts over roughly
// fifteen seconds, which absorbs a rate-limit blip without turning one slow
// turn into a minutes-long stall.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:   4,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      8 * time.Second,
		Jitter:        0.2,
		MaxRetryAfter: DefaultMaxRetryAfter,
	}
}

// jitterFraction is Jitter held to its documented range: a fraction in [0,1].
// Outside it a spread goes negative (a retry before the server's time, or a
// negative wait) or multiplies a wait the server bounded.
func (p RetryPolicy) jitterFraction() float64 {
	switch {
	case !(p.Jitter > 0): // also NaN
		return 0
	case p.Jitter > 1:
		return 1
	}
	return p.Jitter
}

// delayFor computes the backoff before attempt n (1-based), honouring a
// server-supplied Retry-After above all else: never earlier than the server's
// time (bounded by MaxRetryAfter), spread later by the policy's jitter.
//
// rand01 supplies the jitter draw and returns a value in [0,1). It is a
// function rather than a *rand.Rand so that the source can be swapped — a nil
// value means "no jitter", which is what lets a test assert the exact backoff
// sequence — and so that no random-number generator's mutable state is shared
// across the goroutines that call this concurrently.
func (p RetryPolicy) delayFor(attempt int, retryAfter time.Duration, rand01 func() float64) time.Duration {
	if retryAfter > 0 {
		// The server knows when it will be ready. Do not second-guess it — but
		// do bound it, so a hostile or buggy header cannot park the harness.
		ceiling := p.MaxRetryAfter
		if ceiling <= 0 {
			ceiling = DefaultMaxRetryAfter
		}
		if retryAfter > ceiling {
			retryAfter = ceiling
		}
		// Jitter rides on top of the server's time and never subtracts from it.
		// Every client a server rate-limits together is told the same value, and
		// waiting exactly that long brings them all back in the same instant.
		// The spread is taken from the clamped value, after the ceiling and not
		// folded under it: clipping the sum would put every client that was told
		// something larger back on the ceiling together. So the longest wait is
		// the ceiling plus Jitter of it, and a hostile header cannot stretch it.
		// MaxRetryAfter is the operator's to set, so the sum saturates rather
		// than wrapping to an instant retry.
		if j := p.jitterFraction(); j > 0 && rand01 != nil {
			base := min(retryAfter, math.MaxInt64/2)
			spread := time.Duration(rand01() * j * float64(base))
			if spread > math.MaxInt64-retryAfter {
				return math.MaxInt64
			}
			retryAfter += spread
		}
		return retryAfter
	}
	delay := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if max := float64(p.MaxDelay); delay > max {
		delay = max
	}
	if j := p.jitterFraction(); j > 0 && rand01 != nil {
		spread := delay * j
		delay = delay - spread/2 + rand01()*spread
	}
	return time.Duration(delay)
}

// Client is a provider-agnostic HTTP client.
type Client struct {
	Provider string
	BaseURL  string
	HTTP     *http.Client
	Retry    RetryPolicy
	// Header returns the per-request headers, including auth. It is a function
	// so a credential can be refreshed between attempts rather than captured
	// once at construction.
	Header func() (http.Header, error)
	// RequestIDHeader names the response header carrying the provider's trace
	// id, so failures are reportable to that provider's support.
	RequestIDHeader string
	// ResponseHeaderTimeout bounds the wait between sending a request and
	// receiving the response headers. It is a time-to-first-byte bound, not a
	// bound on the response: it stops running the moment headers arrive, so a
	// healthy stream of any length is never cut off by it. Zero disables it,
	// which leaves the caller's context as the only bound.
	//
	// New sets DefaultHostedHeaderTimeout for a hosted endpoint and
	// DefaultFirstTokenTimeout for a loopback one, whose server may send no
	// headers until prefill is done. A timeout is reported as ErrHeaderTimeout
	// and is retryable like any other transport fault.
	ResponseHeaderTimeout time.Duration

	// clock arms the response-header timer. Unexported because production has
	// one clock and nil is it; the seam lets a test hold the timer's callback
	// at the instant the response arrives, which no sleep can do reliably. See
	// StallClock.
	clock StallClock
}

// DefaultHostedHeaderTimeout is how long a hosted provider may take to answer
// with response headers. Hosted streaming APIs send headers as soon as a
// request is accepted, so this only has to clear a slow connection or a
// queueing front end, not generation.
const DefaultHostedHeaderTimeout = DefaultHostedStallTimeout

// ErrHeaderTimeout reports that a server accepted a request and then sent no
// response headers within the client's ResponseHeaderTimeout.
type ErrHeaderTimeout struct {
	After time.Duration
}

func (e *ErrHeaderTimeout) Error() string {
	return fmt.Sprintf("the server sent no response headers within %s of receiving the request; "+
		"the connection was abandoned (raise the response header timeout for a server that "+
		"queues requests before it answers)", e.After)
}

// Timeout reports that this is a timeout, so callers classifying errors treat it
// as one.
func (e *ErrHeaderTimeout) Timeout() bool { return true }

// send issues one request under the client's response-header bound.
//
// The bound is a timer that cancels the request's own derived context, disarmed
// the instant Do returns. Cancelling is the only way to abandon a request that
// is blocked in the runtime's poller, and disarming on return is what keeps the
// bound from ever reaching the body: the derived context outlives Do and is
// released when the caller closes the body, so a long healthy stream is not
// subject to it.
//
// Who won is decided by a claim both sides take under one mutex, not by what
// Stop returns. Stop reports false once the runtime has committed to running
// the callback, which is not the same as the callback having cancelled
// anything: reading that false as "the timer won" discarded a response Do had
// already returned (and, when Stop was never consulted, a late callback
// cancelled the context under a body the caller already held). With the claim, a
// response that arrives first is kept whatever the callback does afterwards,
// and a callback that fires first has cancelled the context before send looks.
func (c *Client) send(ctx context.Context, req *http.Request) (*http.Response, error) {
	limit := c.ResponseHeaderTimeout
	if limit <= 0 {
		return c.HTTP.Do(req)
	}
	reqCtx, cancel := context.WithCancelCause(ctx)
	clock := c.clock
	if clock == nil {
		clock = realTime{}
	}
	var (
		mu      sync.Mutex
		settled bool // Do has returned: the callback may no longer cancel
		expired bool // the callback ran first and has cancelled reqCtx
	)
	timer := clock.AfterFunc(limit, func() {
		mu.Lock()
		defer mu.Unlock()
		if settled {
			return
		}
		expired = true
		cancel(&ErrHeaderTimeout{After: limit})
	})
	resp, err := c.HTTP.Do(req.WithContext(reqCtx))
	mu.Lock()
	settled = true
	timedOut := expired
	mu.Unlock()
	timer.Stop()
	if timedOut {
		// The timer won the race against the headers. The body belongs to a
		// cancelled context and cannot be read; report the timeout.
		if resp != nil {
			_ = resp.Body.Close()
		}
		cause := context.Cause(reqCtx)
		cancel(nil)
		var timeout *ErrHeaderTimeout
		if errors.As(cause, &timeout) {
			return nil, timeout
		}
		if err == nil {
			err = cause
		}
		return nil, err
	}
	if err != nil {
		cancel(nil)
		return nil, err
	}
	resp.Body = &releaseOnClose{ReadCloser: resp.Body, release: func() { cancel(nil) }}
	return resp, nil
}

// releaseOnClose frees a request's derived context when its body is closed.
type releaseOnClose struct {
	io.ReadCloser
	release func()
}

func (r *releaseOnClose) Close() error {
	err := r.ReadCloser.Close()
	r.release()
	return err
}

// LocalLoopbackTransport builds an http.Transport optimized for local loopback
// communication (127.0.0.1, localhost, [::1]). It pools connections with high
// concurrency to eliminate socket churn, sets keep-alives, disables compression
// overhead, and avoids proxy evaluation.
func LocalLoopbackTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil, // Bypass environment proxy lookup for local loopback
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true, // Uncompressed loopback avoids CPU overhead
		ForceAttemptHTTP2:   false,
	}
}

// IsLoopbackURL reports whether a base URL addresses this machine.
//
// Exported because the answer decides more than which connection pool to use.
// An adapter whose provider is *named* "local" has to be able to tell a server
// on this machine from a remote one before it attaches a credential to a
// request: a key an operator set for some other vendor's service must not
// travel to an arbitrary host just because that host was configured here. See
// llm/local.checkCredentialDestination.
//
// A URL that will not parse is treated as non-loopback by the substring
// fallback only when it actually contains a loopback literal, so the failure
// direction is "assume remote", which is the safe one for a caller deciding
// whether to send a secret.
func IsLoopbackURL(rawURL string) bool { return isLoopbackURL(rawURL) }

func isLoopbackURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return strings.Contains(rawURL, "127.0.0.1") || strings.Contains(rawURL, "localhost") || strings.Contains(rawURL, "::1")
	}
	host := u.Hostname()
	if host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "0.0.0.0" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// sharedLoopback is the one loopback pool every client in the process shares.
//
// A transport per client meant a pool per client: a capability probe or an
// endpoint scan builds a client, makes a request or two and drops it, so every
// probe opened fresh connections and abandoned the old ones to idle out — with
// their goroutines — 90 seconds later. Shared, a probe reuses a warm keep-alive
// connection to the same server. The settings are LocalLoopbackTransport's.
var sharedLoopback = sync.OnceValue(LocalLoopbackTransport)

// hostedMaxIdleConnsPerHost is how many idle connections to one hosted
// endpoint survive between requests.
//
// http.DefaultTransport keeps two. A turn that fans out to sixty-four
// sub-agents opens sixty-four connections to the same host, and over HTTP/1.1
// — a plain-http inference box on the LAN, or any endpoint that does not
// negotiate h2 — all but two are closed the moment their response ends, so
// the next round of the fan-out pays sixty-two fresh dials and TLS handshakes
// to reach a server it was connected to a second ago. Sixty-four matches the
// fan-out ceiling and the loopback pool. Over HTTP/2 the setting is inert: one
// connection carries every stream.
const hostedMaxIdleConnsPerHost = 64

// stockDefaultTransport is http.DefaultTransport as this package found it.
var stockDefaultTransport = http.DefaultTransport

// sharedHosted is the pool every hosted client shares while
// http.DefaultTransport is still the standard library's own.
//
// It is a clone, not a fresh Transport, so proxying from the environment, the
// dialer, the TLS and HTTP/2 settings and every timeout are exactly the
// default's; only the idle bound differs. Clone is taken after the default's
// HTTP/2 setup has run, which leaves the clone to configure its own HTTP/2
// rather than share the default's connection pool.
var sharedHosted = sync.OnceValue(func() http.RoundTripper {
	stock, ok := stockDefaultTransport.(*http.Transport)
	if !ok {
		return stockDefaultTransport
	}
	tr := stock.Clone()
	if tr.MaxIdleConnsPerHost < hostedMaxIdleConnsPerHost {
		tr.MaxIdleConnsPerHost = hostedMaxIdleConnsPerHost
	}
	return tr
})

// hostedTransport is the round tripper for a non-loopback endpoint.
//
// A program that replaced http.DefaultTransport — to route through its own
// proxy, record traffic, or pin certificates — chose what every default client
// uses, and these clients used the default before they used a shared pool. So
// a replacement is honoured as it stands, read at construction exactly as
// before; the shared clone is only ever a stand-in for the stock transport.
func hostedTransport() http.RoundTripper {
	if current := http.DefaultTransport; current != stockDefaultTransport {
		return current
	}
	return sharedHosted()
}

// New builds a client with sane defaults.
func New(provider, baseURL string, header func() (http.Header, error)) *Client {
	tr := hostedTransport()
	headerTimeout := DefaultHostedHeaderTimeout
	if isLoopbackURL(baseURL) {
		tr = sharedLoopback()
		// A local server may send no headers until prefill is finished, and
		// prefill scales with prompt length, so the first-token budget is the
		// bound that does not kill a healthy cold request.
		headerTimeout = DefaultFirstTokenTimeout
	}

	return &Client{
		Provider: provider,
		BaseURL:  strings.TrimRight(baseURL, "/"),
		HTTP: &http.Client{
			// No global timeout: streaming responses are long-lived by design,
			// and a client-wide deadline would kill them mid-turn. Callers bound
			// a call with the context instead.
			Timeout:   0,
			Transport: tr,
			// Provider credentials travel in custom headers (x-api-key,
			// x-goog-api-key) that net/http does not strip when a redirect
			// leaves the host, and a 307/308 replays the request body too.
			CheckRedirect: refuseCrossOriginRedirect,
		},
		Retry:                 DefaultRetryPolicy(),
		Header:                header,
		RequestIDHeader:       "request-id",
		ResponseHeaderTimeout: headerTimeout,
	}
}

// ErrCrossOriginRedirect reports that a provider answered with a redirect to a
// different origin, which this client refuses to follow.
//
// The refusal is total rather than filtered by status or header: a key header
// would follow a 307/308 to any host, the body with it, and a 301/302/303 that
// drops the body still carries the key. A provider endpoint that has moved is
// fixed in configuration, not by trusting whatever origin it points at.
type ErrCrossOriginRedirect struct {
	// From and To are origins (scheme and host), never full URLs, so the error
	// cannot carry a query string.
	From, To string
}

func (e *ErrCrossOriginRedirect) Error() string {
	return fmt.Sprintf("the server redirected to another origin (%s -> %s); the request, its credentials "+
		"and its body were not sent there. Point the provider's base URL at the final endpoint", e.From, e.To)
}

// maxRedirects is net/http's own limit on a redirect chain.
const maxRedirects = 10

// ErrTooManyRedirects reports a redirect chain longer than maxRedirects, which
// is a loop or a misconfigured endpoint, not a fault a retry can clear.
type ErrTooManyRedirects struct {
	Limit int
}

func (e *ErrTooManyRedirects) Error() string {
	return fmt.Sprintf("the server redirected more than %d times; the endpoint's base URL needs correcting", e.Limit)
}

// refuseCrossOriginRedirect is the http.Client redirect policy: same-origin
// redirects are followed under net/http's own limit of ten, and any redirect to
// another origin fails closed.
func refuseCrossOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return &ErrTooManyRedirects{Limit: maxRedirects}
	}
	if !sameOrigin(via[0].URL, req.URL) {
		return &ErrCrossOriginRedirect{From: originOf(via[0].URL), To: originOf(req.URL)}
	}
	return nil
}

// sameOrigin reports whether two URLs share scheme, host and port, with the
// scheme's default port and host case treated as the same.
func sameOrigin(a, b *url.URL) bool {
	return originOf(a) == originOf(b)
}

func originOf(u *url.URL) string {
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	return scheme + "://" + net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// transportFailure classifies an error from sending a request.
//
// A refused redirect and an exhausted redirect chain are permanent: sending the
// same request again reaches the same redirect, and a retry loop would ask the
// origin four times to learn it. Both are carried as their own typed error
// rather than as the *url.Error net/http wraps around them, because that
// wrapper's text is `Post "<full URL>"`, which would put the request's query
// string into a message the way ErrCrossOriginRedirect promises not to.
func (c *Client) transportFailure(err error) *Error {
	var refused *ErrCrossOriginRedirect
	if errors.As(err, &refused) {
		return &Error{Provider: c.Provider, Err: refused, permanent: true}
	}
	var loop *ErrTooManyRedirects
	if errors.As(err, &loop) {
		return &Error{Provider: c.Provider, Err: loop, permanent: true}
	}
	return &Error{Provider: c.Provider, Err: err}
}

// Post sends a JSON body and returns the response for the caller to read.
//
// Retries are performed here, which is why the body is marshalled once and
// replayed from memory: a streamed request body cannot be retried, and
// silently sending an empty second attempt is worse than not retrying.
func (c *Client) Post(ctx context.Context, path string, body any) (*http.Response, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Provider: c.Provider, Err: fmt.Errorf("encoding request: %w", err)}
	}
	return c.do(ctx, http.MethodPost, path, payload)
}

// Get retrieves a resource, with the same retry policy, credential refresh and
// error typing as Post.
//
// It exists so capability discovery — asking a server which models it actually
// serves — runs through the one transport that knows about rate limits and
// retryable statuses, rather than through a bare http.Get beside it that would
// have to relearn all of that.
func (c *Client) Get(ctx context.Context, path string) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, path, nil)
}

// do runs one request under the retry policy. A nil payload sends no body,
// which is what distinguishes a GET from a POST here; everything else about
// the two is deliberately identical.
func (c *Client) do(ctx context.Context, method, path string, payload []byte) (*http.Response, error) {
	attempts := c.Retry.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var last *Error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, &Error{Provider: c.Provider, Attempts: attempt - 1, Err: err, cancelled: true}
		}

		finish, denied := c.admitAttempt(ctx, method, path, payload, attempt)
		if denied != nil {
			return nil, denied
		}
		resp, failure := c.attempt(ctx, method, path, payload)
		if err := c.finishAttempt(finish, resp, failure, attempt); err != nil {
			return nil, err
		}
		if failure == nil {
			return resp, nil
		}
		failure.Attempts = attempt
		last = failure

		if !failure.Retryable() || attempt == attempts {
			break
		}

		// rand.Float64 here is the top-level math/rand/v2 function, which is
		// documented safe for concurrent use and draws from per-thread state
		// rather than a shared, lock-free generator. The Client used to carry
		// its own *rand.Rand, which is neither: two turns that both hit a 429
		// drew from it at once, and since DefaultRetryPolicy ships Jitter 0.2
		// that was the default path, not an exotic one. A per-Client generator
		// bought nothing — nothing needs a reproducible jitter sequence — so
		// the state it raced on is simply gone rather than wrapped in a mutex.
		delay := c.Retry.delayFor(attempt, retryAfter(failure), rand.Float64)
		select {
		case <-ctx.Done():
			return nil, &Error{Provider: c.Provider, Attempts: attempt, Err: ctx.Err(), cancelled: true}
		case <-time.After(delay):
		}
	}
	return nil, last
}

func (c *Client) attempt(ctx context.Context, method, path string, payload []byte) (*http.Response, *Error) {
	if err := ctx.Err(); err != nil {
		return nil, &Error{Provider: c.Provider, Err: err, cancelled: true, notSent: true}
	}
	// A nil payload must produce a request with no body at all, not one with an
	// empty reader: some servers reject a GET carrying Content-Type on a
	// zero-length body, and the failure reads as a routing error rather than
	// the malformed request it is.
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, &Error{Provider: c.Provider, Err: err, permanent: true, notSent: true}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if c.Header != nil {
		extra, err := c.Header()
		if err != nil {
			// A credential that cannot be resolved is not a transient failure —
			// retrying it just delays the same error. Marked permanent so the
			// retry loop honours that rather than treating it as the Status-0
			// dial failure it superficially resembles.
			return nil, &Error{Provider: c.Provider, permanent: true, notSent: true,
				Err: fmt.Errorf("resolving credentials: %w", err)}
		}
		for key, values := range extra {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, &Error{Provider: c.Provider, Err: err, cancelled: true, notSent: true}
	}
	resp, err := c.send(ctx, req)
	if err != nil {
		// Distinguish the caller's cancellation from a network fault: the first
		// is a decision, not a failure, and retrying it cannot succeed while
		// the context stays done.
		//
		// The test is the context's own state, not errors.Is(err,
		// context.DeadlineExceeded) on the returned error. Those are not the
		// same question: net's dial timeout reports itself as
		// context.DeadlineExceeded even when the caller's context is untouched,
		// so matching on the error would misclassify the single most retryable
		// fault there is — a server that was slow to accept a connection — as a
		// cancellation and stop trying.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, &Error{Provider: c.Provider, Err: ctxErr, cancelled: true}
		}
		return nil, c.transportFailure(err)
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	return nil, &Error{
		Provider:  c.Provider,
		Status:    resp.StatusCode,
		Body:      strings.TrimSpace(string(snippet)),
		RequestID: resp.Header.Get(c.RequestIDHeader),
		retryHint: resp.Header.Get("Retry-After"),
	}
}

// retryAfter interprets a Retry-After header in either of its documented
// forms: delay-seconds, or an HTTP date.
func retryAfter(e *Error) time.Duration {
	if e == nil || e.retryHint == "" {
		return 0
	}
	hint := strings.TrimSpace(e.retryHint)
	seconds, err := strconv.ParseInt(hint, 10, 64)
	switch {
	case err == nil && seconds >= 0:
		// Saturating: seconds * time.Second wraps negative from 9223372037, and
		// a header asking for the longest wait must not become the shortest.
		if seconds > math.MaxInt64/int64(time.Second) {
			return time.Duration(math.MaxInt64)
		}
		return time.Duration(seconds) * time.Second
	case errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(hint, "-"):
		return time.Duration(math.MaxInt64) // beyond int64, still a very long wait
	}
	if when, err := http.ParseTime(e.retryHint); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// Event is one server-sent event.
type Event struct {
	// Name is the SSE "event:" field. Providers that do not use it leave this
	// empty and carry the discriminator inside Data.
	Name string
	// Data is the accumulated "data:" payload.
	Data []byte
}

// SSE reads a server-sent event stream.
//
// It handles the parts of the format that actually differ between providers:
// some send an "event:" name and put the type there, some send only "data:"
// and put the type inside the JSON, and some terminate with a literal
// "[DONE]" sentinel while others just close the stream.
type SSE struct {
	reader *bufio.Reader
	closer io.Closer
	// Done is the sentinel payload that terminates the stream, e.g. "[DONE]".
	// Empty means the stream ends only at EOF.
	Done string
	err  error

	// doneSeen records that the Done sentinel itself ended the stream, as
	// opposed to the connection simply closing. Both surface from Next as
	// io.EOF; this is how a caller tells a finished stream from a truncated
	// one. See Terminated.
	doneSeen bool

	// watchdog bounds silence, not the length of the stream.
	//
	// The client has no overall timeout on purpose — a streaming response is
	// long-lived by design — which left the turn deadline as the only bound. On
	// a local server that is not enough: a server that emits one token and then
	// wedges on a Metal command buffer or an evicted model burns the entire
	// turn budget in silence. Silence and a long turn are different conditions,
	// and only one of them is a fault.
	watchdog *stallWatchdog

	// payload records whether the line currently being read carries a field
	// rather than a comment or padding. The watchdog is re-armed from inside
	// the read, so it needs to know whether the bytes arriving are output or
	// heartbeat; see readLine.
	//
	// It is written and read only on the goroutine inside Next, because the
	// reads it guards happen synchronously beneath that call.
	payload bool

	// closeMu makes Close idempotent and safe to call from two goroutines. The
	// obvious use of an exported Close — a reader goroutine and a cancellation
	// path both tearing the stream down — had them writing the closer field at
	// once, and closing an arbitrary io.Closer twice is not something this
	// package gets to assume is harmless.
	closeMu  sync.Mutex
	closed   bool
	closeErr error
}

// StallClock is the stall watchdog's view of time.
//
// It is a seam because the alternative is a test that cannot tell this
// package's defect from the machine it runs on. A stream paced with real
// sleeps reaches the reader as "no bytes for longer than the limit" in two
// completely different situations — the watchdog moved its deadline wrongly,
// which is the defect, and the scheduler simply did not run the sender for
// that long, which is a statement about how busy the runner is — and from
// inside the test the two are the same error. Measured on this repository: the
// openaicompat case that asserts a slow-but-live stream survives failed in six
// of eight `go test ./...` runs and in none of five runs on its own. Widening
// the limit only moves the load at which that recurs.
//
// A driven clock removes the ambiguity rather than reducing its probability:
// the gap between frames becomes a number the test states, so the assertion
// can only fail for the first reason.
//
// Production has one implementation and reaches it by passing a nil clock.
// Nothing outside a test is expected to supply one.
type StallClock interface {
	// Now reports the current time, which must not run backwards.
	Now() time.Time
	// AfterFunc schedules f to run once d has passed, and returns a handle
	// that can re-arm or cancel it.
	AfterFunc(d time.Duration, f func()) StallTimer
}

// StallTimer is the part of *time.Timer the watchdog uses.
type StallTimer interface {
	Reset(d time.Duration) bool
	Stop() bool
}

// realTime is the clock a nil StallClock resolves to, and the only one that
// ships.
type realTime struct{}

func (realTime) Now() time.Time { return time.Now() }

func (realTime) AfterFunc(d time.Duration, f func()) StallTimer { return time.AfterFunc(d, f) }

// stallWatchdog closes the body when the stream has been silent for too long.
//
// It closes rather than signalling because the read is blocked inside the
// runtime's network poller; closing the body is what unblocks it. The reported
// error is replaced afterwards so the caller sees a stall rather than the
// "use of closed network connection" that closing produces.
//
// It runs two limits in sequence, because time-to-first-token and the gap
// between tokens are different quantities and one budget cannot bound both.
// Prefill on a local server scales with prompt length — measured at two minutes
// for a 14.7k-token prompt on a 4-bit 27B, so a 40k-token prompt legitimately
// exceeds five — while a gap of that size *between* tokens means the generation
// has stopped. Sharing one budget killed real turns on exactly the workload
// this harness exists for, then told the operator to go restart a healthy
// server.
type stallWatchdog struct {
	mu     sync.Mutex
	timer  StallTimer
	closer io.Closer

	// clock is where every instant below comes from. See StallClock.
	clock StallClock

	// firstToken bounds the wait for the first byte of output; idle bounds
	// every gap after that. limit is whichever is currently in force.
	firstToken time.Duration
	idle       time.Duration
	limit      time.Duration

	// deadline, not the timer, is the authority on when this stream is
	// considered stalled. The timer is only a wake-up hint: see progress.
	deadline time.Time
	// armed is when the timer is currently scheduled to fire.
	armed time.Time

	// sawOutput records that the stream has produced at least one payload
	// byte, which is what moves it from the first-token limit to the idle one.
	sawOutput bool

	tripped bool
	// trippedAfter and trippedCold are captured at the moment of tripping
	// rather than read back later, so the reported error names the limit that
	// actually expired and the phase it expired in.
	trippedAfter time.Duration
	trippedCold  bool
	stopped      bool
}

func newStallWatchdog(closer io.Closer, firstToken, idle time.Duration, clock StallClock) *stallWatchdog {
	if clock == nil {
		clock = realTime{}
	}
	w := &stallWatchdog{
		closer:     closer,
		clock:      clock,
		firstToken: firstToken,
		idle:       idle,
		limit:      firstToken,
	}
	w.deadline = clock.Now().Add(firstToken)
	w.armed = w.deadline
	w.timer = clock.AfterFunc(firstToken, w.fire)
	return w
}

// fire is the timer callback. It may wake for a deadline that has since moved,
// which is the whole reason it re-reads the deadline instead of tripping on
// sight: an AfterFunc timer that has already fired can still be re-armed by a
// concurrent progress() holding the mutex, and the previous version of this
// code then ran the callback twice and closed the body twice — harmless on an
// http.Response.Body, not harmless on the arbitrary io.Closer the exported
// constructor accepts, and enough to mark a stream that had just made progress
// as stalled.
func (w *stallWatchdog) fire() {
	w.mu.Lock()
	if w.stopped || w.tripped {
		w.mu.Unlock()
		return
	}
	if remaining := w.deadline.Sub(w.clock.Now()); remaining > 0 {
		// Progress arrived while this callback was waiting for the mutex, or
		// progress moved the deadline without paying for a timer reset. Either
		// way this stream is not stalled; sleep for what is left.
		w.armed = w.deadline
		w.timer.Reset(remaining)
		w.mu.Unlock()
		return
	}
	w.tripped = true
	w.trippedAfter = w.limit
	w.trippedCold = !w.sawOutput
	closer := w.closer
	w.mu.Unlock()
	if closer != nil {
		_ = closer.Close()
	}
}

// progress restarts the clock. It is called for every read that delivers bytes
// of a payload line, not once per completed line: a single data: line can carry
// a whole tool-call argument list, and a stream that is delivering one
// continuously is the opposite of stalled.
func (w *stallWatchdog) progress() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped || w.tripped {
		return
	}
	if !w.sawOutput {
		// The stream has proved it can produce output, so prefill is over and
		// the tight limit takes over from here.
		w.sawOutput = true
		w.limit = w.idle
	}
	now := w.clock.Now()
	w.deadline = now.Add(w.limit)

	// Re-arming the runtime timer on every read would put a timer-heap
	// operation on the hot path of a fast local stream. It is not needed:
	// deadline above is what fire() consults, and fire() reschedules itself
	// when it wakes early. So touch the timer only when the deadline has moved
	// backwards — which happens when the first-token limit gives way to the
	// shorter idle one, and would otherwise leave the timer sleeping past it —
	// or forwards by more than an eighth of the limit. That bounds the timer
	// churn to eight operations per limit window, and bounds how late a stall
	// is noticed to that same eighth.
	if drift := w.deadline.Sub(w.armed); drift < 0 || drift > w.limit/8 {
		w.armed = w.deadline
		w.timer.Reset(w.deadline.Sub(now))
	}
}

func (w *stallWatchdog) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stopped = true
	w.timer.Stop()
}

// stalled returns the failure to report, or nil if this stream has not been
// abandoned.
func (w *stallWatchdog) stalled() *ErrStalled {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.tripped {
		return nil
	}
	return &ErrStalled{Idle: w.trippedAfter, BeforeFirstToken: w.trippedCold}
}

// ErrStalled reports that a stream went silent without ending.
type ErrStalled struct {
	// Idle is the limit that expired.
	Idle time.Duration
	// BeforeFirstToken records that not one byte of output ever arrived, which
	// is a different fault from a generation that started and then stopped and
	// sends an operator somewhere else entirely.
	BeforeFirstToken bool
}

// Error names the silence and stops there.
//
// It used to end "This is not a slow model — it is a stopped one", which this
// layer cannot know: it sees an open connection and no bytes, and a cold
// prefill and a wedged process are identical from here. That sentence sent an
// operator to restart a server that was working correctly on a large prompt.
func (e *ErrStalled) Error() string {
	if e.BeforeFirstToken {
		return fmt.Sprintf("the stream produced no output at all for %s and was abandoned; "+
			"the request was accepted and the connection stayed open, but not one token "+
			"arrived. The most common cause is a prompt large enough that prefill has not "+
			"finished — that time scales with prompt length, and on a local model it is "+
			"minutes, not seconds — and the next most common is a server that accepted the "+
			"request and wedged. Check whether the server process is still doing work "+
			"before restarting it, and raise the first-token limit if the prompt is large", e.Idle)
	}
	return fmt.Sprintf("the stream produced no output for %s between tokens and was "+
		"abandoned; it had already emitted output, then the connection stayed open and "+
		"nothing more came. Check the server process and whether the model is still "+
		"resident", e.Idle)
}

// Timeout reports that this is a timeout, so callers classifying errors treat it
// as one.
func (e *ErrStalled) Timeout() bool { return true }

// DefaultFirstTokenTimeout bounds the wait for the first byte of output.
//
// It is far larger than any inter-token limit because it is bounding a
// different thing: prefill, whose cost scales with prompt length. At the rate
// measured on this project — 120s for a 14,738-token prompt on a 4-bit 27B,
// about 8ms per token — this covers a prompt of roughly 110k tokens, which is
// past the point where the harness compacts. It is deliberately kept to half
// the 30-minute turn budget rather than raised further: a limit that never
// fires before the turn deadline is not a diagnosis, and the value of firing
// first is that the operator is told the stream went silent instead of that the
// turn ran out of time.
const DefaultFirstTokenTimeout = 15 * time.Minute

// DefaultHostedStallTimeout is the gap between tokens a hosted provider is
// allowed once it has started producing them. Hosted APIs stream steadily or
// not at all, so this only has to be past the longest legitimate pause between
// deltas, not past a cold model load.
const DefaultHostedStallTimeout = 2 * time.Minute

// MaxFrameBytes bounds a single SSE line.
//
// The bound exists because the alternative is unbounded: a server that never
// sends a newline, or a corrupted connection that eats one, otherwise grows one
// byte slice until the process dies, and "out of memory" is not a diagnosis of
// anything. The value is chosen to be far above any legitimate frame rather
// than close to one — a local model's tool call carrying a whole file is
// megabytes, and a full 200k-token completion is under one — so exceeding it
// means the stream is broken, not that the model was verbose.
const MaxFrameBytes = 32 << 20

// ErrOversizedFrame reports a single SSE line that exceeded MaxFrameBytes.
//
// It is an error rather than a truncation on purpose: handing the decoder half
// a JSON object would surface as a provider bug, and an operator would go
// looking for one.
type ErrOversizedFrame struct {
	Limit int
}

func (e *ErrOversizedFrame) Error() string {
	return fmt.Sprintf("a single stream frame exceeded %d bytes without ending; "+
		"the stream was abandoned rather than buffered further. A frame this large "+
		"means the server is not terminating its lines, not that the response was long",
		e.Limit)
}

// MaxEventBytes bounds one assembled SSE event — every data: line from the
// start of the event to the blank line that dispatches it.
//
// MaxFrameBytes was the only bound here and it bounds the wrong unit. It caps a
// *line*, and Next appends each data: line onto the event's payload, so a
// server sending well-formed short lines and never a blank one grew that
// payload for as long as it kept talking while every individual line stayed
// orders of magnitude under the line cap. Measured: 134,219,775 bytes in one
// event and 175 MiB of heap, four times MaxFrameBytes, and still climbing when
// the generator stopped. Nothing above catches it — payloadReader re-arms the
// stall watchdog on every byte that arrives, so the stream reads as healthy,
// and each adapter's decode cap is consulted only after a frame decodes, which
// never happens while the event is still being assembled.
//
// It is the same number as MaxFrameBytes rather than a larger one because the
// two bound the same thing at different granularity: one line at the limit is
// already a whole event's worth, so a stream that needs more than this is
// broken either way, and a second constant to keep in step would only be a
// second thing to get wrong.
const MaxEventBytes = MaxFrameBytes

// ErrOversizedEvent reports an SSE event whose accumulated data: lines exceeded
// MaxEventBytes before a blank line dispatched it.
//
// It is a distinct type from ErrOversizedFrame because it is a distinct fault
// and the two send an operator to different places: an oversized frame means
// the server is not terminating its lines, while this means it terminates them
// and never ends the event.
type ErrOversizedEvent struct {
	Limit int
}

func (e *ErrOversizedEvent) Error() string {
	return fmt.Sprintf("a single stream event accumulated more than %d bytes of data "+
		"without the blank line that ends it; the stream was abandoned rather than "+
		"buffered further. The server is terminating its lines but never terminating "+
		"the event, which is a broken stream, not a long response", e.Limit)
}

// MaxDecodedResponseBytes bounds how much of one response a stream decoder will
// hold, across every accumulator it has open.
//
// It lives here, with the other bounds on a streaming response, because three
// adapters were each declaring their own copy of the same number and then
// disagreeing about what it counted — gemini omitted thought signatures from
// the tally that anthropic included, and none of them counted the per-call
// bookkeeping at all. One constant and one stated rule is the only arrangement
// under which "the response exceeded the decode limit" means the same thing on
// every provider.
//
// 4MiB is the value because the largest legitimate response is bounded by the
// model's output cap, and even a 128k-token completion at four bytes a token is
// about 512KiB. Exceeding it is an error, never a truncation: settling 4MiB of
// a runaway as though it were the answer is silent corruption.
const MaxDecodedResponseBytes = 4 << 20

// RetainedAccumulatorBytes is what a decoder charges against
// MaxDecodedResponseBytes for each accumulator it is holding open, on top of
// the bytes that accumulator has collected.
//
// An accumulator is not free. Every one of them costs a struct, a
// strings.Builder, and an entry in each index the decoder keys it by — in
// openaicompat that is four maps and a slice. A stream of tool-call fragments
// that each carry a fresh id and *zero* argument bytes therefore allocated one
// of these per fragment while adding nothing to a tally that counted only
// content: measured at 400,000 accumulators and 98 MiB of heap with
// decodedBytes() still reporting 0 and the cap never firing. A cap that counts
// only the bytes the server chose to send is a cap the server chooses whether
// to be bound by.
//
// 256 bytes is measured rather than guessed: the 98 MiB above over 400,000
// accumulators is roughly 245 bytes each. Charging it means a runaway of empty
// calls trips the same 4MiB ceiling as a runaway of content, at about sixteen
// thousand open calls — far past any real response, which opens a handful.
const RetainedAccumulatorBytes = 256

// NewSSEWithStall wraps a response body and abandons it after silence, using
// DefaultFirstTokenTimeout for the wait before the first token. A non-positive
// idle disables the watchdog entirely, which is the documented escape hatch.
//
// A nil clock means real time, which is what every caller outside a test
// passes. See StallClock for why the parameter exists at all.
func NewSSEWithStall(body io.ReadCloser, done string, idle time.Duration, clock StallClock) *SSE {
	if idle <= 0 {
		return NewSSE(body, done)
	}
	// max, not the default outright: an operator who asked for a longer
	// inter-token limit than the first-token default meant a slower server, and
	// silently giving them a *shorter* budget for the slowest phase of the
	// request would be the opposite of what they asked for.
	firstToken := DefaultFirstTokenTimeout
	if idle > firstToken {
		firstToken = idle
	}
	return NewSSEWithLimits(body, done, firstToken, idle, clock)
}

// NewSSEWithLimits wraps a response body with both limits stated.
//
// It exists so a caller that knows the prompt size can size the first-token
// allowance to it, rather than inheriting a constant chosen for the worst case.
// A non-positive value for either limit disables the watchdog, and a nil clock
// means real time.
func NewSSEWithLimits(body io.ReadCloser, done string, firstToken, idle time.Duration, clock StallClock) *SSE {
	s := NewSSE(body, done)
	if firstToken > 0 && idle > 0 && body != nil {
		s.watchdog = newStallWatchdog(body, firstToken, idle, clock)
	}
	return s
}

// NewSSE wraps a response body.
func NewSSE(body io.ReadCloser, done string) *SSE {
	s := &SSE{closer: body, Done: done}
	// The payloadReader sits under the buffer, not over it, so the watchdog is
	// re-armed by bytes arriving from the socket rather than by lines being
	// assembled out of the buffer.
	//
	// A large buffer because a single data: line can carry a whole content
	// block; the default 4KB limit would fail on ordinary traffic.
	// The pool only ever holds *bufio.Reader of sseReaderSize; anything else
	// would be a bug elsewhere, and a fresh buffer is the safe answer to it.
	reader, ok := sseReaders.Get().(*bufio.Reader)
	if !ok {
		reader = bufio.NewReaderSize(nil, sseReaderSize)
	}
	s.reader = reader
	s.reader.Reset(&payloadReader{src: body, sse: s})
	return s
}

// sseReaderSize is the read buffer behind every stream. See NewSSE.
const sseReaderSize = 1 << 20

// sseReaders recycles stream read buffers.
//
// Every streamed turn allocated a fresh mebibyte, zeroed, to read a reply that
// is usually a few kilobytes, and dropped it when the turn ended — so a
// fan-out of short sub-agent turns was mostly a garbage generator. A buffer is
// returned only by release, at the one point a stream provably never reads
// again; see there.
var sseReaders = sync.Pool{New: func() any { return bufio.NewReaderSize(nil, sseReaderSize) }}

// release hands the read buffer back once this stream has settled.
//
// It is called only from Next, on the goroutine reading the stream, once s.err
// is set. That is the whole safety argument, and it rests on three facts that
// hold in this file:
//
//   - s.err is sticky. Next returns it on entry without touching the reader,
//     and nothing clears it, so no later call can read the buffer.
//   - Nothing else reads the buffer. Close and the stall watchdog touch only
//     the body, never s.reader, so a Close from a cancellation path racing a
//     Next in flight cannot be using the buffer when this runs, nor after.
//   - Nothing Next returned points into it. readLine's window is consumed
//     before the next read: the event name is copied into a string and data
//     is accumulated into a slice Next allocates.
//
// A stream abandoned before it settles — closed early, or dropped — keeps its
// buffer for the collector exactly as before. Reset to nil drops the pooled
// reader's reference to this stream's body.
func (s *SSE) release() {
	if s.reader == nil {
		return
	}
	s.reader.Reset(nil)
	sseReaders.Put(s.reader)
	s.reader = nil
}

// payloadReader reports byte arrival to the stall watchdog, and puts the bytes
// into the one shape the line parser reads.
//
// It only counts bytes that belong to a payload line. That distinction is the
// whole point: a proxy emitting ": ping" heartbeats delivers bytes forever
// while the stream produces nothing, and counting those as progress turns the
// watchdog into a device that guarantees the turn budget is spent in full.
//
// The event-stream grammar allows a leading UTF-8 byte order mark and three line
// endings (CRLF, LF and a bare CR); the parser above this reads LF-terminated
// lines only. Normalising here, once, under the buffer, keeps that parser's
// fast path (one ReadSlice per line) and means a BOM or a bare CR is handled
// wherever a chunk boundary happens to fall.
type payloadReader struct {
	src io.Reader
	sse *SSE

	// bomDecided is set once the stream's first bytes have been judged. A byte
	// order mark is only a mark at the very start of a stream.
	bomDecided bool
	// skipLF is set after a chunk that ended in CR, which was passed on as LF.
	// A LF opening the next chunk is the other half of a CRLF and is dropped.
	skipLF bool
}

// utf8BOM is the byte order mark the grammar says to discard.
const utf8BOM = "\xEF\xBB\xBF"

// maxEmptyReads is how many consecutive (0, nil) reads the source gets before
// the stream gives up on it. It is bufio's own bound; the reads here sit under
// a bufio.Reader that would report io.ErrNoProgress after the same number, and
// this layer must not turn that guard into a spin.
const maxEmptyReads = 100

func (r *payloadReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for empty := 0; empty < maxEmptyReads; empty++ {
		n, err := r.src.Read(p)
		if !r.bomDecided && n > 0 {
			n, err = r.dropBOM(p, n, err)
		}
		if n > 0 {
			// One scan for the CR that most streams never contain; see endLines.
			if first := bytes.IndexByte(p[:n], '\r'); first >= 0 || r.skipLF {
				n = r.endLines(p[:n], first)
			}
			if n > 0 && r.sse.payload && r.sse.watchdog != nil {
				r.sse.watchdog.progress()
			}
		}
		if n > 0 || err != nil {
			return n, err
		}
	}
	return 0, io.ErrNoProgress
}

// dropBOM removes a leading byte order mark from the first bytes of the stream.
// It reads on while what has arrived is a proper prefix of the mark, because a
// mark split across reads is still a mark; nothing else ever waits here, since
// a stream whose first byte is not 0xEF is decided at once.
func (r *payloadReader) dropBOM(p []byte, n int, err error) (int, error) {
	if len(p) < len(utf8BOM) {
		r.bomDecided = true
		return n, err
	}
	for empty := 0; n < len(utf8BOM) && err == nil && string(p[:n]) == utf8BOM[:n]; {
		var m int
		m, err = r.src.Read(p[n:])
		n += m
		switch {
		case m > 0:
			empty = 0
		case err == nil:
			if empty++; empty >= maxEmptyReads {
				err = io.ErrNoProgress
			}
		}
	}
	r.bomDecided = true
	if n >= len(utf8BOM) && string(p[:len(utf8BOM)]) == utf8BOM {
		n = copy(p, p[len(utf8BOM):n])
	}
	return n, err
}

// endLines rewrites a bare CR as LF, in place, and returns the new length;
// first is the index of the first CR in p, or -1 if there is none.
//
// A CR that opens a CRLF is left for the parser, which already trims it, so a
// CRLF stream is not copied at all; a bare CR is overwritten where it stands. A
// CR that ends the chunk cannot be told from a bare one until the next read, so
// it becomes LF now and the LF that may open the next chunk is dropped by
// skipLF: the one case that shifts bytes, and it needs a CRLF split exactly at a
// chunk boundary.
func (r *payloadReader) endLines(p []byte, first int) int {
	if r.skipLF {
		r.skipLF = false
		if p[0] == '\n' {
			p = p[:copy(p, p[1:])]
			first = bytes.IndexByte(p, '\r')
		}
	}
	for i := first; i >= 0; {
		switch {
		case i+1 == len(p):
			p[i] = '\n'
			r.skipLF = true
			return len(p)
		case p[i+1] == '\n':
			i += 2
		default:
			p[i] = '\n'
			i++
		}
		next := bytes.IndexByte(p[i:], '\r')
		if next < 0 {
			break
		}
		i += next
	}
	return len(p)
}

// Terminated reports whether the stream ended because the Done sentinel
// arrived, rather than because the connection closed.
//
// Next returns io.EOF for both, and a decoder that treats every io.EOF as a
// finished stream settles a truncated one as a success: the half-received tool
// call runs and the usage is recorded as zero. A decoder whose provider has a
// sentinel reads this at EOF; a provider that ends with a terminal event of its
// own (anthropic's message_stop, gemini's interaction.completed) tracks that
// event itself and reports a missing one with TruncatedStream.
//
// Always false for a stream built without a Done sentinel.
func (s *SSE) Terminated() bool { return s.doneSeen }

// isDone reports whether an assembled event payload is the Done sentinel.
func (s *SSE) isDone(data []byte) bool {
	return s.Done != "" && string(bytes.TrimSpace(data)) == s.Done
}

// Next returns the next event, or io.EOF when the stream ends.
func (s *SSE) Next() (Event, error) {
	if s.err != nil {
		return Event{}, s.err
	}
	event, err := s.next()
	if s.err != nil {
		s.release()
	}
	return event, err
}

// next reads one event. Next wraps it so the buffer goes back the moment the
// stream settles, whichever of next's many returns settled it.
func (s *SSE) next() (Event, error) {
	var event Event
	var data []byte

	for {
		line, err := s.readLine()
		if s.watchdog != nil {
			if stalled := s.watchdog.stalled(); stalled != nil {
				// The body was closed under the read. Report why, rather than
				// the "use of closed network connection" that closing produced.
				s.err = stalled
				return Event{}, s.err
			}
		}
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) && len(data) > 0 {
				// A final event without its terminating blank line.
				event.Data = data
				if s.isDone(data) {
					s.doneSeen = true
					s.err = io.EOF
					return Event{}, io.EOF
				}
				return event, nil
			}
			s.err = err
			return Event{}, err
		}

		trimmed := bytes.TrimRight(line, "\r\n")

		// A blank line dispatches the accumulated event.
		if len(trimmed) == 0 {
			if len(data) == 0 && event.Name == "" {
				continue // keep-alive or padding
			}
			event.Data = data
			if s.isDone(data) {
				s.doneSeen = true
				s.err = io.EOF
				return Event{}, io.EOF
			}
			return event, nil
		}

		// Comments are keep-alives.
		if trimmed[0] == ':' {
			continue
		}

		field, value, found := bytes.Cut(trimmed, []byte(":"))
		if !found {
			field, value = trimmed, nil
		}
		value = bytes.TrimPrefix(value, []byte(" "))

		switch string(field) {
		case "event":
			event.Name = string(value)
		case "data":
			// Checked before the append, not after, so the oversized copy is
			// never made. See MaxEventBytes: the per-line bound in readLine
			// cannot see this growth, because every line involved is legal.
			grown := len(data) + len(value)
			if len(data) > 0 {
				grown++
			}
			if grown > MaxEventBytes {
				s.err = &ErrOversizedEvent{Limit: MaxEventBytes}
				return Event{}, s.err
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			data = append(data, value...)
		}

		if err != nil {
			if errors.Is(err, io.EOF) && (len(data) > 0 || event.Name != "") {
				event.Data = data
				s.err = io.EOF
				if s.isDone(data) {
					s.doneSeen = true
					return Event{}, io.EOF
				}
				return event, nil
			}
			s.err = err
			return Event{}, err
		}
	}
}

// readLine returns the next line including its terminator, bounded by
// MaxFrameBytes.
//
// It classifies the line before consuming it so the watchdog knows whether the
// bytes about to arrive are output or padding. Only a field line counts as
// progress: a comment is a keep-alive by definition, and a blank line with
// nothing accumulated is padding. Everything else — data:, event:, id:, retry:
// — is content this parser acts on, and a server sending any of it is a server
// that is still working.
func (s *SSE) readLine() ([]byte, error) {
	s.payload = false
	if s.watchdog != nil {
		// Peek blocks until the first byte of the line is available, and that
		// wait is deliberately unguarded by the payload flag: waiting for the
		// first byte of the next line is exactly the silence being measured.
		head, err := s.reader.Peek(1)
		if err != nil {
			// Returned, never dropped. Peek *takes* the pending read error out
			// of the buffer when it reports it, so reading again does not
			// rediscover it — it produces a second, worse error from a
			// connection that is already dead. On a cancelled turn that
			// replaces the context.Canceled net/http supplies with "use of
			// closed network connection", and a caller that tells a cancelled
			// turn from a transport fault then retries what the user stopped.
			return nil, err
		}
		if isPayloadStart(head[0]) {
			s.payload = true
			s.watchdog.progress()
		}
	}

	var line []byte
	for {
		// ReadSlice rather than ReadBytes so the growth is ours to bound;
		// ErrBufferFull means the line is longer than the buffer, not that
		// anything went wrong.
		chunk, err := s.reader.ReadSlice('\n')
		if len(line)+len(chunk) > MaxFrameBytes {
			return nil, &ErrOversizedFrame{Limit: MaxFrameBytes}
		}
		if line == nil && !errors.Is(err, bufio.ErrBufferFull) {
			// The whole line fit in the buffer, which is nearly every line, so
			// it is returned as the window ReadSlice gave. That window is only
			// valid until the next read — Next copies what it keeps (the event
			// name, the data it accumulates) before reading again, so a copy
			// here was one allocation per streamed line for nothing.
			return chunk, err
		}
		// A line longer than the buffer is assembled from successive windows,
		// each of which the next read overwrites, so those are copied.
		line = append(line, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, err
	}
}

// isPayloadStart reports whether a line beginning with this byte carries a
// field rather than a comment or a line terminator.
func isPayloadStart(b byte) bool { return b != ':' && b != '\n' && b != '\r' }

// Close releases the underlying body. It is idempotent and safe to call from
// more than one goroutine.
func (s *SSE) Close() error {
	if s.watchdog != nil {
		s.watchdog.stop()
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.closer != nil {
		s.closeErr = s.closer.Close()
	}
	return s.closeErr
}

// Probe makes one request to an absolute URL, with this client's headers and
// without the retry policy.
//
// It exists for server-native endpoints that sit outside the API root — Ollama's
// /api/show, llama.cpp's /props — which a caller reaches by trimming the
// OpenAI-compatible suffix off the base URL. Retrying is deliberately omitted:
// a probe's most common answer is 404, because these endpoints are
// server-specific and most servers are not the one being probed, and retrying a
// definite negative four times just makes startup slower.
func (c *Client) Probe(ctx context.Context, method, url string, payload []byte) (*http.Response, error) {
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, &Error{Provider: c.Provider, Err: err}
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Header != nil {
		extra, err := c.Header()
		if err != nil {
			return nil, &Error{Provider: c.Provider, permanent: true,
				Err: fmt.Errorf("resolving credentials: %w", err)}
		}
		for key, values := range extra {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}
	resp, err := c.send(ctx, req)
	if err != nil {
		return nil, c.transportFailure(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, &Error{Provider: c.Provider, Status: resp.StatusCode,
			Err: fmt.Errorf("probe returned HTTP %d", resp.StatusCode)}
	}
	return resp, nil
}

// StreamAccepted is what a caller returns from the accept callback of
// PostStream: the body to read the stream from, or a failure to judge.
type StreamAccepted struct {
	// Body is the reader the caller should go on to decode. It is usually the
	// response body itself, or that body with whatever the callback consumed
	// while inspecting it put back in front.
	Body io.ReadCloser
}

// PostStream issues a streaming request and lets the caller reject a response
// the transport itself considers successful, retrying on the same policy that
// governs a transport failure.
//
// It exists because a 200 is not a success on every wire. Gemini's interactions
// endpoint reports an overloaded model, and at least some transient backend
// faults, as an `event: error` frame inside an otherwise healthy stream —
// status 200, headers sent, then the failure. Measured live on 2026-08-19:
// "gemini-3.7-flash is currently experiencing high demand" and "Invalid input
// received." both arrived that way, the second of which was proved transient by
// replaying the identical request afterwards and having it accepted.
//
// Classifying on status alone made every one of those fatal to the whole turn,
// so a harness with a deliberate four-attempt policy retried none of the
// failures its provider actually produces. That is the gap this closes: the
// judgement of what counts as a failure moves to the caller, which is the only
// layer that can read the wire's own vocabulary, while the backoff, the jitter,
// the attempt ceiling and the Retry-After handling stay here in one place.
//
// The callback must only report a failure it is safe to retry from — that is,
// before anything has been handed to the caller's own consumer. Re-issuing a
// request whose first half has already been read would duplicate content.
func (c *Client) PostStream(
	ctx context.Context,
	path string,
	body any,
	accept func(*http.Response) (StreamAccepted, *Error),
) (io.ReadCloser, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &Error{Provider: c.Provider, Err: fmt.Errorf("encoding request: %w", err)}
	}

	attempts := c.Retry.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}

	var last *Error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, &Error{Provider: c.Provider, Attempts: attempt - 1, Err: err, cancelled: true}
		}

		finish, denied := c.admitAttempt(ctx, http.MethodPost, path, payload, attempt)
		if denied != nil {
			return nil, denied
		}
		resp, failure := c.attempt(ctx, http.MethodPost, path, payload)
		if failure == nil {
			if accept == nil {
				if err := c.finishAttempt(finish, resp, nil, attempt); err != nil {
					return nil, err
				}
				return resp.Body, nil
			}
			accepted, rejected := accept(resp)
			if rejected == nil {
				if err := c.finishAttempt(finish, resp, nil, attempt); err != nil {
					if accepted.Body != nil {
						_ = accepted.Body.Close()
					}
					return nil, err
				}
				return accepted.Body, nil
			}
			// The body is spent either way: the callback has read into it, and
			// a retry issues a fresh request rather than resuming this one.
			resp.Body.Close()
			failure = rejected
		}
		if err := c.finishAttempt(finish, resp, failure, attempt); err != nil {
			return nil, err
		}
		failure.Attempts = attempt
		last = failure

		if !failure.Retryable() || attempt == attempts {
			break
		}
		delay := c.Retry.delayFor(attempt, retryAfter(failure), rand.Float64)
		select {
		case <-ctx.Done():
			return nil, &Error{Provider: c.Provider, Attempts: attempt, Err: ctx.Err(), cancelled: true}
		case <-time.After(delay):
		}
	}
	return nil, last
}

// StreamFailure builds the error a PostStream callback returns when the wire
// reports a failure inside a successful response.
//
// Status is carried so Retryable() answers the same way it would for the same
// condition delivered as a status code — a provider that says "overloaded" in a
// frame means what a 503 means, and the two must not be classified differently
// for having arrived by different routes.
func StreamFailure(provider, message string, status int, retryable bool) *Error {
	e := &Error{Provider: provider, Status: status, Body: message,
		Err: errors.New(message), inStream: true}
	if retryable && !retryableStatus(status) {
		// Named as a service condition so Retryable() agrees with the caller's
		// reading without this type having to know any provider's vocabulary.
		e.Status = http.StatusServiceUnavailable
	}
	if !retryable {
		e.Status = http.StatusBadRequest
	}
	return e
}

// InStream reports whether this failure arrived inside a response the transport
// had already accepted. It is carried so a report can say so: "the request
// failed" and "the request was accepted and then the stream said no" send an
// operator to different places.
func (e *Error) InStream() bool { return e != nil && e.inStream }
