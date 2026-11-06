package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// cancellationsIn returns the notifications/cancelled frames among frames.
func cancellationsIn(t *testing.T, frames []string) []int64 {
	t.Helper()
	var ids []int64
	for _, f := range frames {
		var n struct {
			Method string `json:"method"`
			Params struct {
				RequestID int64 `json:"requestId"`
			} `json:"params"`
		}
		if json.Unmarshal([]byte(f), &n) == nil && n.Method == "notifications/cancelled" {
			ids = append(ids, n.Params.RequestID)
		}
	}
	return ids
}

// A reply that dispatch already delivered must reach the caller even though
// the context ended in the same instant. select picks at random among ready
// cases, so without a single arbiter of who owns the reply the caller was told
// "timed out" for a call that had succeeded, sent a cancellation for a request
// it held the answer to, and a retrying caller ran a non-idempotent tool twice.
func TestReadyReplyIsNotLostToCancelledContext(t *testing.T) {
	for i := 0; i < 200; i++ {
		c, in := newWiredClient()
		ctx, cancel := context.WithCancel(context.Background())
		c.beforeWait = func(id int64, respCh chan *Response) {
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"v":7}}`))
			if len(respCh) != 1 {
				t.Errorf("reply was not delivered into the channel")
			}
			cancel()
		}
		var out struct {
			V int `json:"v"`
		}
		err := c.Call(ctx, "tools/call", nil, &out)
		cancel()
		if err != nil || out.V != 7 {
			t.Fatalf("iteration %d: reply in hand was forgotten: err=%v out=%+v", i, err, out)
		}
		if got := cancellationsIn(t, in.frames()); len(got) != 0 {
			t.Fatalf("iteration %d: cancelled a request whose reply was in hand: %v", i, in.frames())
		}
	}
}

// The same rule for a server that exited with the reply already delivered.
func TestReadyReplyIsNotLostToServerExit(t *testing.T) {
	for i := 0; i < 200; i++ {
		c, _ := newWiredClient()
		c.beforeWait = func(id int64, respCh chan *Response) {
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
			close(c.doneCh)
		}
		if err := c.Call(context.Background(), "tools/call", nil, nil); err != nil {
			t.Fatalf("iteration %d: reply in hand was forgotten for the exit: %v", i, err)
		}
	}
}

// The other order: the context ends first and wins, the cancellation is
// written naming the request, and the reply that comes later is recorded and
// handed to nobody.
func TestReplyAfterCancelIsRecordedNotDelivered(t *testing.T) {
	c, in := newWiredClient()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out struct {
		V int `json:"v"`
	}
	err := c.Call(ctx, "tools/call", nil, &out)
	if err == nil || !strings.Contains(err.Error(), context.Canceled.Error()) {
		t.Fatalf("want the context error, got %v", err)
	}
	if got := cancellationsIn(t, in.frames()); len(got) != 1 || got[0] != 1 {
		t.Fatalf("want one cancellation of request 1, got %v from %q", got, in.frames())
	}
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"v":9}}`))
	if !strings.Contains(strings.Join(c.Diagnostics(), "\n"), "request 1, which is no longer pending") {
		t.Fatalf("late reply was not recorded: %v", c.Diagnostics())
	}
	if out.V != 0 {
		t.Fatalf("late reply was delivered to the abandoned call: %+v", out)
	}
	if len(c.pending) != 0 {
		t.Fatalf("pending entries remain: %d", len(c.pending))
	}
}

// An inbound notifications/cancelled is accepted, recorded, and never answered.
func TestInboundCancelledIsRecordedAndNotAnswered(t *testing.T) {
	c, in := newWiredClient()
	c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1,"reason":"user gave up"}}`))
	if fr := in.frames(); len(fr) != 0 {
		t.Fatalf("a notification was answered: %q", fr)
	}
	d := strings.Join(c.Diagnostics(), "\n")
	if !strings.Contains(d, `cancelled its request "1"`) || !strings.Contains(d, "user gave up") {
		t.Fatalf("inbound cancellation was not recorded with its id and reason: %q", d)
	}
}

// requestId in an inbound cancellation names a request the server issued, not
// one of ours ("only reference requests previously issued in the same
// direction"), so it must not abort a client Call that happens to share the id.
func TestInboundCancelledDoesNotAbortClientCall(t *testing.T) {
	c, _ := newWiredClient()
	c.beforeWait = func(id int64, respCh chan *Response) {
		go func() {
			c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
			time.Sleep(20 * time.Millisecond)
			c.dispatch([]byte(`{"jsonrpc":"2.0","id":1,"result":{"v":3}}`))
		}()
	}
	var out struct {
		V int `json:"v"`
	}
	if err := c.Call(context.Background(), "tools/call", nil, &out); err != nil || out.V != 3 {
		t.Fatalf("call was aborted by an inbound cancellation: err=%v out=%+v", err, out)
	}
}

// The record keeps the newest entries and says how many older ones went.
func TestDiagnosticsKeepNewestAndCountDropped(t *testing.T) {
	c, _ := newWiredClient()
	for i := 0; i < maxServerErrors+7; i++ {
		c.recordError(fmt.Sprintf("line %d", i))
	}
	d := c.Diagnostics()
	if len(d) != maxServerErrors || d[len(d)-1] != fmt.Sprintf("line %d", maxServerErrors+6) {
		t.Fatalf("want the newest %d entries, got %d ending %q", maxServerErrors, len(d), d[len(d)-1])
	}
	if s := c.diagnosticsSuffix(); !strings.Contains(s, "7 earlier entries dropped") {
		t.Fatalf("suffix does not report the dropped count: %q", s)
	}
}

// Repeated inbound cancellations fold into one entry with a count, and any
// control characters in server text are escaped at the single recording owner.
func TestCancelledRunsCoalesceAndRecordsAreEscaped(t *testing.T) {
	c, _ := newWiredClient()
	for i := 0; i < 5; i++ {
		c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`))
	}
	if d := c.Diagnostics(); len(d) != 1 || !strings.Contains(d[0], "repeated 5 times") {
		t.Fatalf("want one coalesced entry, got %q", d)
	}
	c.recordError("stderr\nforged\x1b[31m")
	for _, d := range c.Diagnostics() {
		if strings.ContainsAny(d, "\n\x1b") {
			t.Fatalf("raw control characters recorded: %q", d)
		}
	}
}
