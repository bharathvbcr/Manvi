package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// Adversarial tests for the withdraw/claimedReply arbitration and the inbound
// notifications/cancelled recorder. Every Call here runs under callWithin so a
// hang in claimedReply is a failed test with a stack, not a stuck suite.

func callWithin(t *testing.T, bound time.Duration, c *Client, ctx context.Context, method string, out any) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, method, nil, out) }()
	select {
	case err := <-done:
		return err
	case <-time.After(bound):
		t.Fatalf("Call(%s) did not return within %s: claimedReply is blocked on a channel nobody will fill", method, bound)
		return nil
	}
}

func hasCancelFrame(t *testing.T, frames []string) int {
	t.Helper()
	return len(cancellationsIn(t, frames))
}

// A second reply for the same id must not block the read loop (the channel has
// room for one) and must not be delivered to anyone.
func TestAdvDuplicateReplyNeitherBlocksNorDoubleDelivers(t *testing.T) {
	c, _ := newWiredClient()
	loopDone := make(chan struct{})
	c.beforeWait = func(id int64, respCh chan *Response) {
		go func() {
			defer close(loopDone)
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"v":1}}`))
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"v":2}}`))
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":"1","result":{"v":3}}`))
		}()
	}
	var out struct {
		V int `json:"v"`
	}
	if err := callWithin(t, 5*time.Second, c, context.Background(), "tools/call", &out); err != nil || out.V != 1 {
		t.Fatalf("want first reply, got err=%v out=%+v", err, out)
	}
	select {
	case <-loopDone:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch of a duplicate reply blocked the read loop")
	}
	if n := strings.Count(strings.Join(c.Diagnostics(), "\n"), "no longer pending"); n != 2 {
		t.Fatalf("want both duplicates recorded as no longer pending, got %d: %v", n, c.Diagnostics())
	}
}

// failAllPending racing a cancelled context, from both sides, many times. The
// reply (synthetic error) is claimed by whoever deletes first; the invariant is
// that a cancel frame is sent iff the caller returned a context error.
func TestAdvFailAllPendingVersusCancelNeverHangsOrContradicts(t *testing.T) {
	for i := 0; i < 300; i++ {
		c, in := newWiredClient()
		ctx, cancel := context.WithCancel(context.Background())
		start := make(chan struct{})
		c.beforeWait = func(id int64, respCh chan *Response) {
			go func() { <-start; c.failAllPending("boom") }()
			go func() { <-start; cancel() }()
			close(start)
		}
		err := callWithin(t, 5*time.Second, c, ctx, "tools/call", nil)
		cancel()
		if err == nil {
			t.Fatalf("iteration %d: failAllPending produced no error", i)
		}
		ctxErr := errors.Is(err, context.Canceled)
		// failAllPending may still be sending; settle before reading frames.
		time.Sleep(time.Millisecond)
		n := hasCancelFrame(t, in.frames())
		switch {
		case ctxErr && n != 1:
			t.Fatalf("iteration %d: context error but %d cancel frames", i, n)
		case !ctxErr && n != 0:
			t.Fatalf("iteration %d: reply was claimed (%v) yet %d cancel frames were sent", i, err, n)
		}
	}
}

// The end-of-stream flush closes the channel; a Call whose ctx ended at the same
// moment must return (closed-connection error) rather than hang, via the real
// readLoop.
func TestAdvEOFFlushVersusCancelNeverHangs(t *testing.T) {
	for i := 0; i < 200; i++ {
		pr, pw := io.Pipe()
		c, in := newWiredClient()
		c.stdout = pr
		c.stderrDone = make(chan struct{})
		close(c.stderrDone)
		ctx, cancel := context.WithCancel(context.Background())
		c.beforeWait = func(id int64, respCh chan *Response) {
			go c.readLoop()
			go func() { _ = pw.Close() }()
			go cancel()
		}
		err := callWithin(t, 5*time.Second, c, ctx, "tools/call", nil)
		cancel()
		if err == nil {
			t.Fatalf("iteration %d: expected an error", i)
		}
		<-c.doneCh
		n := hasCancelFrame(t, in.frames())
		// A ctx error may legitimately carry no frame here: Notify refuses once
		// the server's stdout has ended. More than one is never right.
		if n > 1 {
			t.Fatalf("iteration %d: %d cancel frames", i, n)
		}
		if !errors.Is(err, context.Canceled) && n != 0 {
			t.Fatalf("iteration %d: server-side end (%v) but %d cancel frames", i, err, n)
		}
	}
}

// Concurrent real reply vs ctx cancel, nothing arranged: err==nil XOR one cancel
// frame, never both, never neither.
func TestAdvReplyVersusCancelExactlyOneOutcome(t *testing.T) {
	var delivered, cancelled int
	for i := 0; i < 500; i++ {
		c, in := newWiredClient()
		ctx, cancel := context.WithCancel(context.Background())
		go_ := make(chan struct{})
		c.beforeWait = func(id int64, respCh chan *Response) {
			go func() { <-go_; c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"v":7}}`)) }()
			go func() { <-go_; cancel() }()
			close(go_)
		}
		var out struct {
			V int `json:"v"`
		}
		err := callWithin(t, 5*time.Second, c, ctx, "tools/call", &out)
		cancel()
		n := hasCancelFrame(t, in.frames())
		switch {
		case err == nil && out.V == 7 && n == 0:
			delivered++
		case err != nil && errors.Is(err, context.Canceled) && n == 1:
			cancelled++
		default:
			t.Fatalf("iteration %d: err=%v out=%+v cancelFrames=%d (reply and cancel both acted on, or neither)", i, err, out, n)
		}
	}
	t.Logf("delivered=%d cancelled=%d", delivered, cancelled)
}

// Deadline expiry, caller cancel: one cancel frame naming the request, with the
// context error as reason. initialize is never cancelled.
func TestAdvCancelFrameSentForTimeoutAndCancelNotForInitialize(t *testing.T) {
	c, in := newWiredClient()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := callWithin(t, 5*time.Second, c, ctx, "tools/call", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline error, got %v", err)
	}
	if got := cancellationsIn(t, in.frames()); len(got) != 1 || got[0] != 1 {
		t.Fatalf("want exactly one cancel for request 1, got %v", in.frames())
	}

	c2, in2 := newWiredClient()
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_ = callWithin(t, 5*time.Second, c2, ctx2, "initialize", nil)
	if n := hasCancelFrame(t, in2.frames()); n != 0 {
		t.Fatalf("initialize was cancelled: %q", in2.frames())
	}
}

// initializeAs on its doneCh branch with the reply already claimed returns the
// reply; with nothing claimed it returns the exit error and sends no cancel. Run
// concurrently with the dispatch to shake the withdraw/receive pairing.
func TestAdvInitializeReplyVersusExitNeverHangs(t *testing.T) {
	for i := 0; i < 300; i++ {
		c, in := newWiredClient()
		c.cfg.Name = "t"
		go_ := make(chan struct{})
		go func() {
			<-go_
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18","capabilities":{},"serverInfo":{"name":"s","version":"1"}}}`))
		}()
		go func() { <-go_; close(c.doneCh) }()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f := &initFlight{abort: make(chan struct{})}
			close(go_)
			_, _ = c.initializeAs(context.Background(), f, "2025-06-18")
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: initializeAs hung", i)
		}
		if n := hasCancelFrame(t, in.frames()); n != 0 {
			t.Fatalf("iteration %d: initialize cancelled", i)
		}
	}
}

// A reply with an id spelled differently from the one issued ("01", "+1", " 1 ")
// is not the id the client sent. Spec: the response id MUST be the request id.
func TestAdvLooseStringIDsAreNotMatched(t *testing.T) {
	for _, spelled := range []string{`"01"`, `"+1"`, `" 1 "`, `"0x1"`} {
		c, _ := newWiredClient()
		c.beforeWait = func(id int64, respCh chan *Response) {
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":` + spelled + `,"result":{}}`))
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := callWithin(t, 5*time.Second, c, ctx, "tools/call", nil)
		cancel()
		if err == nil {
			t.Errorf("reply with id %s was delivered to request 1", spelled)
		}
	}
}

// Inbound cancellation payloads that must be recorded, bounded, never answered,
// and never touch pending.
func TestAdvInboundCancelledHostilePayloads(t *testing.T) {
	big := strings.Repeat("A", 10<<20)
	payloads := map[string]string{
		"huge reason":        `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"` + big + `"}}`,
		"non-string reason":  `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":{"a":[1,2]}}}`,
		"numeric reason":     `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":42}}`,
		"object requestId":   `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":{"x":1}}}`,
		"array requestId":    `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":[1,2,3]}}`,
		"null requestId":     `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":null}}`,
		"huge number":        `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":123456789012345678901234567890123456789}}`,
		"huge requestId str": `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"` + big + `"}}`,
		"missing params":     `{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
		"params string":      `{"jsonrpc":"2.0","method":"notifications/cancelled","params":"x"}`,
		"params array":       `{"jsonrpc":"2.0","method":"notifications/cancelled","params":[1]}`,
		"params null":        `{"jsonrpc":"2.0","method":"notifications/cancelled","params":null}`,
		"invalid utf8":       "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":1,\"reason\":\"\xff\xfe\xc3\"}}",
	}
	for name, p := range payloads {
		t.Run(name, func(t *testing.T) {
			c, in := newWiredClient()
			respCh := make(chan *Response, 1)
			c.pending[1] = respCh
			c.dispatch([]byte(p))
			if fr := in.frames(); len(fr) != 0 {
				t.Fatalf("answered a notification: %q", fr)
			}
			if _, ok := c.pending[1]; !ok {
				t.Fatalf("a server-side cancellation removed the client's pending request")
			}
			if len(respCh) != 0 {
				t.Fatalf("a server-side cancellation delivered into a client call")
			}
			for _, d := range c.Diagnostics() {
				if len(d) > 1024 {
					t.Fatalf("recorded diagnostic is %d bytes, unbounded", len(d))
				}
			}
		})
	}
}

// The reason is attacker-controlled text that is later joined into error
// messages. Newlines and ANSI escapes must not survive into the record, or a
// server can forge extra "the server reported" lines / repaint a terminal.
func TestAdvInboundCancelledReasonIsNotLogInjection(t *testing.T) {
	c, _ := newWiredClient()
	c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"a\nFAKE: tool approved","reason":"x\nmcp: forged line\u001b[31mred\u001b[0m\r\u0000"}}`))
	for _, d := range c.Diagnostics() {
		if strings.ContainsAny(d, "\n\r\x1b\x00") {
			t.Fatalf("diagnostic carries raw control characters from the server: %q", d)
		}
	}
	if len(c.Diagnostics()) == 0 {
		t.Fatal("nothing recorded")
	}
}

// recordError stops at 100 entries and never makes room, so a server that floods
// notifications/cancelled can fill the record with noise and every later real
// diagnostic (the one explaining a death) is dropped. diagnosticsSuffix then
// quotes the flood instead of the cause.
func TestAdvCancelledFloodDoesNotCrowdOutLaterDiagnostics(t *testing.T) {
	c, _ := newWiredClient()
	for i := 0; i < 10000; i++ {
		c.dispatch([]byte(fmt.Sprintf(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":%d,"reason":"spam"}}`, i)))
	}
	if n := len(c.Diagnostics()); n > 100 {
		t.Fatalf("diagnostics grew unbounded: %d", n)
	}
	c.recordError("REAL CAUSE: fatal: token rejected")
	if !strings.Contains(c.diagnosticsSuffix(), "REAL CAUSE") {
		t.Fatalf("the real diagnostic was dropped behind the cancellation flood: %q", c.diagnosticsSuffix())
	}
}

// Concurrent flood from many goroutines plus real Calls: no race, no hang.
func TestAdvCancelledFloodConcurrentWithCalls(t *testing.T) {
	c, _ := newWiredClient()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"r"}}`))
			}
		}()
	}
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		_ = callWithin(t, 5*time.Second, c, ctx, "tools/call", nil)
		cancel()
	}
	wg.Wait()
}
