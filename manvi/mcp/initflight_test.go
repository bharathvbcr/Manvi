package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests drive the handshake through the wired client of lifecycle_test.go:
// the test plays the server by feeding dispatch the frames a real stdout would
// carry, and reads what the client wrote from the recording stdin. That makes
// every ordering below deterministic — "the server answers after the first
// caller gave up" is a line of the test, not a sleep.

const initReplyBody = `"result":{"protocolVersion":"2025-11-25","capabilities":{},"serverInfo":{"name":"s","version":"1"}}`

func initReply(id int) []byte {
	return []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(id) + `,` + initReplyBody + `}`)
}

// countMethod counts the frames the client wrote for one method.
func countMethod(in *recordingStdin, method string) int {
	n := 0
	for _, f := range in.frames() {
		var probe struct {
			Method string `json:"method"`
		}
		if json.Unmarshal([]byte(f), &probe) == nil && probe.Method == method {
			n++
		}
	}
	return n
}

// waitForMethod blocks until the client has written want frames of method.
func waitForMethod(t *testing.T, in *recordingStdin, method string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for countMethod(in, method) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d %q frame(s); wrote %q", want, method, in.frames())
		}
		time.Sleep(time.Millisecond)
	}
}

// currentFlight returns the handshake in progress, or nil.
func currentFlight(c *Client) *initFlight {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	return c.initFlight
}

func wiredForInit(t *testing.T) (*Client, *recordingStdin) {
	t.Helper()
	c, in := newWiredClient()
	t.Cleanup(func() { _ = c.Close() })
	return c, in
}

// initCall runs Initialize on its own goroutine and delivers the outcome.
type initOutcome struct {
	res *InitializeResult
	err error
}

func initAsync(c *Client, ctx context.Context) <-chan initOutcome {
	out := make(chan initOutcome, 1)
	go func() {
		res, err := c.Initialize(ctx)
		out <- initOutcome{res, err}
	}()
	return out
}

func receive(t *testing.T, ch <-chan initOutcome, what string) initOutcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return", what)
		return initOutcome{}
	}
}

// endTransport does what readLoop does when stdout ends: it fails every pending
// request and closes doneCh.
func endTransport(c *Client) {
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.pendingMu.Unlock()
	close(c.doneCh)
}

// The first caller gives up before the server answers; the server then answers.
// A second caller must adopt that answer. Re-sending initialize while the first
// is outstanding is a second handshake on one session, and the late reply to
// the first would be discarded as "no longer pending".
func TestInitializeIsSingleFlightAcrossACallersTimeout(t *testing.T) {
	c, in := wiredForInit(t)

	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel1()
	if _, err := c.Initialize(ctx1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first caller: err = %v, want its own deadline", err)
	}

	second := initAsync(c, withTimeout(t, 2*time.Second))
	// The frame is written after the pending entry is registered, so seeing it
	// proves the reply has somewhere to land.
	waitForMethod(t, in, "initialize", 1)
	// The server answers late, after the first caller is gone.
	c.dispatch(initReply(1))

	o := receive(t, second, "second caller")
	if o.err != nil || o.res == nil {
		t.Fatalf("second caller did not adopt the late reply: res=%v err=%v frames=%q", o.res, o.err, in.frames())
	}
	if n := countMethod(in, "initialize"); n != 1 {
		t.Fatalf("initialize was sent %d times, want exactly 1: %q", n, in.frames())
	}
	if n := countMethod(in, "notifications/initialized"); n != 1 {
		t.Fatalf("notifications/initialized was sent %d times, want exactly 1: %q", n, in.frames())
	}
	if n := countMethod(in, "notifications/cancelled"); n != 0 {
		t.Fatalf("initialize was cancelled: %q", in.frames())
	}
}

func withTimeout(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

// A waiter whose context ends gets its own context error, and the handshake it
// was waiting on carries on and completes for the next caller.
func TestAWaitersCancellationDoesNotDisturbTheHandshake(t *testing.T) {
	c, in := wiredForInit(t)

	ctxA, cancelA := context.WithCancel(context.Background())
	a := initAsync(c, ctxA)
	waitForMethod(t, in, "initialize", 1)
	cancelA()
	if o := receive(t, a, "cancelled waiter"); !errors.Is(o.err, context.Canceled) || o.res != nil {
		t.Fatalf("cancelled waiter: res=%v err=%v, want context.Canceled", o.res, o.err)
	}

	b := initAsync(c, withTimeout(t, 5*time.Second))
	c.dispatch(initReply(1))
	if o := receive(t, b, "next caller"); o.err != nil || o.res == nil {
		t.Fatalf("handshake did not complete for the next caller: res=%v err=%v", o.res, o.err)
	}
	if n := countMethod(in, "initialize"); n != 1 {
		t.Fatalf("initialize was sent %d times, want 1: %q", n, in.frames())
	}
	if n := countMethod(in, "notifications/initialized"); n != 1 {
		t.Fatalf("notifications/initialized sent %d times, want 1", n)
	}
	if n := countMethod(in, "notifications/cancelled"); n != 0 {
		t.Fatalf("a waiter's cancellation cancelled initialize: %q", in.frames())
	}
}

// A reply nobody is waiting for is still the server's answer: the session is
// initialized, and the next call must not send initialize again.
func TestALateInitializeReplyWithNoWaiterIsAdopted(t *testing.T) {
	c, in := wiredForInit(t)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Initialize(ctx); err == nil {
		t.Fatal("initialize with no reply succeeded")
	}
	waitForMethod(t, in, "initialize", 1)
	c.dispatch(initReply(1))
	waitForMethod(t, in, "notifications/initialized", 1)

	res, err := c.Initialize(withTimeout(t, time.Second))
	if err != nil || res == nil || res.ProtocolVersion != "2025-11-25" {
		t.Fatalf("next call after an adopted reply: res=%v err=%v", res, err)
	}
	if n := countMethod(in, "initialize"); n != 1 {
		t.Fatalf("initialize re-sent after the late reply was adopted: %q", in.frames())
	}
	if n := countMethod(in, "notifications/initialized"); n != 1 {
		t.Fatalf("notifications/initialized sent %d times, want 1", n)
	}
	for _, d := range c.Diagnostics() {
		if strings.Contains(d, "no longer pending") {
			t.Fatalf("the initialize reply was discarded as not pending: %v", c.Diagnostics())
		}
	}
}

// An error reply resolves the handshake definitively, so the next call may
// start a new one: exactly one new initialize frame, and no initialized for the
// failed attempt.
func TestAFailedHandshakeMayBeRetriedOnceResolved(t *testing.T) {
	c, in := wiredForInit(t)

	first := initAsync(c, withTimeout(t, 5*time.Second))
	waitForMethod(t, in, "initialize", 1)
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"warming up"}}`))
	o := receive(t, first, "first caller")
	var rpc *RPCError
	if !errors.As(o.err, &rpc) || rpc.Message != "warming up" {
		t.Fatalf("first caller: err = %v, want the server's error", o.err)
	}
	if n := countMethod(in, "notifications/initialized"); n != 0 {
		t.Fatalf("initialized was sent for a failed handshake: %q", in.frames())
	}

	second := initAsync(c, withTimeout(t, 5*time.Second))
	waitForMethod(t, in, "initialize", 2)
	c.dispatch(initReply(2))
	if o := receive(t, second, "retrying caller"); o.err != nil || o.res == nil {
		t.Fatalf("retry after an error reply failed: res=%v err=%v", o.res, o.err)
	}
	if n := countMethod(in, "initialize"); n != 2 {
		t.Fatalf("initialize frames = %d, want 2 (one per resolved attempt): %q", n, in.frames())
	}
	if n := countMethod(in, "notifications/initialized"); n != 1 {
		t.Fatalf("notifications/initialized sent %d times, want 1", n)
	}
}

// Many callers on one outstanding handshake send one frame, and the transport
// ending releases every one of them with an error and ends the flight.
func TestTheServerDyingMidHandshakeReleasesEveryWaiter(t *testing.T) {
	c, in := wiredForInit(t)

	const waiters = 4
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for range waiters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.Initialize(withTimeout(t, 30*time.Second))
			errs <- err
		}()
	}
	waitForMethod(t, in, "initialize", 1)
	// Let every caller reach the shared wait before the server dies.
	deadline := time.Now().Add(5 * time.Second)
	for currentFlight(c) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	flight := currentFlight(c)
	if flight == nil {
		t.Fatal("no handshake in flight")
	}

	endTransport(c)

	released := make(chan struct{})
	go func() { wg.Wait(); close(released) }()
	select {
	case <-released:
	case <-time.After(10 * time.Second):
		t.Fatal("waiters were not released when the server died")
	}
	close(errs)
	for err := range errs {
		if err == nil {
			t.Fatal("a waiter was told the handshake succeeded though the server died")
		}
	}
	select {
	case <-flight.done:
	default:
		t.Fatal("the handshake goroutine did not finish")
	}
	if n := countMethod(in, "initialize"); n != 1 {
		t.Fatalf("initialize sent %d times for %d callers, want 1", n, waiters)
	}
	if currentFlight(c) != nil {
		t.Fatal("a dead handshake is still registered as in flight")
	}
}

// Close releases a handshake that no reply will ever finish, so a server that
// never answers does not leave a goroutine behind for the life of the process.
func TestCloseReleasesAnOutstandingHandshake(t *testing.T) {
	c, in := newWiredClient()

	w := initAsync(c, withTimeout(t, 30*time.Second))
	waitForMethod(t, in, "initialize", 1)
	flight := currentFlight(c)
	if flight == nil {
		t.Fatal("no handshake in flight")
	}

	_ = c.Close()

	if o := receive(t, w, "waiter"); o.err == nil {
		t.Fatal("a waiter was told the handshake succeeded on a closed client")
	}
	select {
	case <-flight.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the handshake goroutine outlived Close")
	}
	if _, err := c.Initialize(withTimeout(t, time.Second)); err == nil {
		t.Fatal("Initialize succeeded on a closed client")
	}
}

// failingNotify is a stdin that accepts everything except the initialized
// notification.
type failingNotify struct{ recordingStdin }

func (f *failingNotify) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "notifications/initialized") {
		return 0, errors.New("broken pipe")
	}
	return f.recordingStdin.Write(p)
}

// The server answered, so a second initialize on the stream is not allowed; a
// failed notification ends the session's handshake rather than being retried
// with a new initialize.
func TestAFailedInitializedNotificationDoesNotReinitialize(t *testing.T) {
	in := &failingNotify{}
	c, _ := newWiredClient()
	c.stdin = in
	t.Cleanup(func() { _ = c.Close() })

	first := initAsync(c, withTimeout(t, 5*time.Second))
	waitForMethod(t, &in.recordingStdin, "initialize", 1)
	c.dispatch(initReply(1))
	o := receive(t, first, "first caller")
	if o.err == nil || !strings.Contains(o.err.Error(), "broken pipe") {
		t.Fatalf("first caller: err = %v, want the notification failure", o.err)
	}
	if _, err := c.Initialize(withTimeout(t, time.Second)); err == nil {
		t.Fatal("a session whose initialized notification failed reported a clean handshake")
	}
	if n := countMethod(&in.recordingStdin, "initialize"); n != 1 {
		t.Fatalf("initialize re-sent after the server had answered: %d frames", n)
	}
}
