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
	if e.Err != nil {
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
	// Jitter is the fraction of the delay randomised, in [0,1]. Without it,
	// every client that failed together retries together.
	Jitter float64
	// MaxRetryAfter bounds how long a server-supplied Retry-After can park the
	// harness. It is an absolute ceiling rather than a multiple of MaxDelay:
	// the two answer different questions, and deriving one from the other makes
	// a short backoff silently shorten the limit on how long we will honour a
	// server's own instruction. Zero means the default.
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

// delayFor computes the backoff before attempt n (1-based), honouring a
// server-supplied Retry-After above all else.
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
			return ceiling
		}
		return retryAfter
	}
	delay := float64(p.BaseDelay) * math.Pow(2, float64(attempt-1))
	if max := float64(p.MaxDelay); delay > max {
		delay = max
	}
	if p.Jitter > 0 && rand01 != nil {
		spread := delay * p.Jitter
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

// New builds a client with sane defaults.
func New(provider, baseURL string, header func() (http.Header, error)) *Client {
	var tr http.RoundTripper = http.DefaultTransport
	if isLoopbackURL(baseURL) {
		tr = LocalLoopbackTransport()
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
		},
		Retry:           DefaultRetryPolicy(),
		Header:          header,
		RequestIDHeader: "request-id",
	}
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

		resp, failure := c.attempt(ctx, method, path, payload)
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
		return nil, &Error{Provider: c.Provider, Err: err}
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
			return nil, &Error{Provider: c.Provider, permanent: true,
				Err: fmt.Errorf("resolving credentials: %w", err)}
		}
		for key, values := range extra {
			for _, value := range values {
				req.Header.Add(key, value)
			}
		}
	}

	resp, err := c.HTTP.Do(req)
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
		return nil, &Error{Provider: c.Provider, Err: err}
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
	if seconds, err := strconv.Atoi(strings.TrimSpace(e.retryHint)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
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
	timer  *time.Timer
	closer io.Closer

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

func newStallWatchdog(closer io.Closer, firstToken, idle time.Duration) *stallWatchdog {
	w := &stallWatchdog{
		closer:     closer,
		firstToken: firstToken,
		idle:       idle,
		limit:      firstToken,
	}
	w.deadline = time.Now().Add(firstToken)
	w.armed = w.deadline
	w.timer = time.AfterFunc(firstToken, w.fire)
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
	if remaining := time.Until(w.deadline); remaining > 0 {
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
	w.deadline = time.Now().Add(w.limit)

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
		w.timer.Reset(time.Until(w.deadline))
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
func NewSSEWithStall(body io.ReadCloser, done string, idle time.Duration) *SSE {
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
	return NewSSEWithLimits(body, done, firstToken, idle)
}

// NewSSEWithLimits wraps a response body with both limits stated.
//
// It exists so a caller that knows the prompt size can size the first-token
// allowance to it, rather than inheriting a constant chosen for the worst case.
// A non-positive value for either limit disables the watchdog.
func NewSSEWithLimits(body io.ReadCloser, done string, firstToken, idle time.Duration) *SSE {
	s := NewSSE(body, done)
	if firstToken > 0 && idle > 0 && body != nil {
		s.watchdog = newStallWatchdog(body, firstToken, idle)
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
	s.reader = bufio.NewReaderSize(&payloadReader{src: body, sse: s}, 1<<20)
	return s
}

// payloadReader reports byte arrival to the stall watchdog.
//
// It only counts bytes that belong to a payload line. That distinction is the
// whole point: a proxy emitting ": ping" heartbeats delivers bytes forever
// while the stream produces nothing, and counting those as progress turns the
// watchdog into a device that guarantees the turn budget is spent in full.
type payloadReader struct {
	src io.Reader
	sse *SSE
}

func (r *payloadReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 && r.sse.payload && r.sse.watchdog != nil {
		r.sse.watchdog.progress()
	}
	return n, err
}

// Next returns the next event, or io.EOF when the stream ends.
func (s *SSE) Next() (Event, error) {
	if s.err != nil {
		return Event{}, s.err
	}

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
			if err == io.EOF && len(data) > 0 {
				// A final event without its terminating blank line.
				event.Data = data
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
			if s.Done != "" && string(bytes.TrimSpace(data)) == s.Done {
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
			if err == io.EOF && (len(data) > 0 || event.Name != "") {
				event.Data = data
				s.err = io.EOF
				if s.Done != "" && string(bytes.TrimSpace(data)) == s.Done {
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
		// Copied out rather than returned as-is: ReadSlice hands back a window
		// into the buffer that the next read invalidates, and every caller here
		// expects the stable slice ReadBytes used to give them.
		line = append(line, chunk...)
		if err == bufio.ErrBufferFull {
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
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &Error{Provider: c.Provider, Err: err}
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

		resp, failure := c.attempt(ctx, http.MethodPost, path, payload)
		if failure == nil {
			if accept == nil {
				return resp.Body, nil
			}
			accepted, rejected := accept(resp)
			if rejected == nil {
				return accepted.Body, nil
			}
			// The body is spent either way: the callback has read into it, and
			// a retry issues a fresh request rather than resuming this one.
			resp.Body.Close()
			failure = rejected
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
