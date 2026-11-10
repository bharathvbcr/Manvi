package codingagent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// sendRawWriter stands in for the agent's stdin. Its Write runs onWrite (which
// may fail the connection, as the reader does when the agent answers with a
// frame it refuses) and then reports result, or blocks until release closes.
type sendRawWriter struct {
	onWrite func()
	result  error
	release chan struct{}
}

func (w *sendRawWriter) Write(p []byte) (int, error) {
	w.onWrite()
	if w.release != nil {
		<-w.release
		return 0, errors.New("pipe closed")
	}
	if w.result != nil {
		return 0, w.result
	}
	return len(p), nil
}

func (w *sendRawWriter) Close() error { return nil }

var errRefusedAnswer = errors.New("managed Antigravity frame omits jsonrpc version")

func sendRawConnection(t *testing.T, w *sendRawWriter) *connection {
	t.Helper()
	ctx, fail := context.WithCancelCause(t.Context())
	t.Cleanup(func() { fail(nil) })
	c := &connection{ctx: ctx, fail: fail, cancel: func() { fail(nil) }, stdin: w}
	if w.onWrite == nil {
		w.onWrite = func() { fail(errRefusedAnswer) }
	}
	return c
}

// A frame the write delivered is reported delivered even when the connection
// fails in the same instant (the agent read it and answered with a refused
// frame). The failure is the next read's to report. Before the fix select
// picked between the two ready cases at random, so StartTurn failed on some
// runs of TestZadvW16MalformedMidTurnFramesFailTheTurnNeverHangOrSucceed.
func TestSendRawReportsADeliveredFrameWhenTheConnectionFailsDuringTheWrite(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := sendRawConnection(t, &sendRawWriter{})
		if err := c.sendRaw(t.Context(), []byte(`{}`)); err != nil {
			t.Fatalf("run %d: a delivered frame reported %v", i, err)
		}
		if got := c.failure(); !errors.Is(got, errRefusedAnswer) {
			t.Fatalf("run %d: the connection's failure is %v, want the refused answer", i, got)
		}
	}
}

// The same holds when the caller passes the connection's own context, as Agy's
// automatic permission answers do: both contexts end together, and the
// caller-cancel case must not pre-empt the write's result.
func TestSendRawOnTheConnectionsOwnContextReportsADeliveredFrame(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := sendRawConnection(t, &sendRawWriter{})
		if err := c.sendRaw(c.ctx, []byte(`{}`)); err != nil {
			t.Fatalf("run %d: a delivered frame reported %v", i, err)
		}
	}
}

// A write that did not deliver, on a connection that failed, reports why the
// connection failed rather than the pipe's error, whichever ready case select
// takes. It runs many times because the pre-fix code lost the failure only
// when the write's case won.
func TestSendRawReportsTheConnectionFailureWhenTheWriteAlsoFails(t *testing.T) {
	for i := 0; i < 500; i++ {
		c := sendRawConnection(t, &sendRawWriter{result: errors.New("broken pipe")})
		if err := c.sendRaw(t.Context(), []byte(`{}`)); !errors.Is(err, errRefusedAnswer) {
			t.Fatalf("run %d: sendRaw = %v, want the connection's failure", i, err)
		}
	}
}

// A plain caller cancel with no connection failure still cancels the
// connection and reports the caller's error.
func TestSendRawCallerCancelStillCancelsTheConnection(t *testing.T) {
	w := &sendRawWriter{release: make(chan struct{}), onWrite: func() {}}
	t.Cleanup(func() { close(w.release) })
	c := sendRawConnection(t, w)
	ctx, cancel := context.WithCancel(t.Context())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := c.sendRaw(ctx, []byte(`{}`)); !errors.Is(err, context.Canceled) {
		t.Fatalf("sendRaw = %v, want the caller's cancel", err)
	}
	if c.ctx.Err() == nil {
		t.Fatal("a caller cancel left the connection running")
	}
	if c.failure() != nil {
		t.Fatalf("a plain cancel recorded a failure: %v", c.failure())
	}
}

// A write still blocked when the connection fails is waited for only up to
// writeSettle: sendRaw then reports the failure and never hangs.
func TestSendRawDoesNotWaitPastWriteSettleForABlockedWrite(t *testing.T) {
	w := &sendRawWriter{release: make(chan struct{})}
	t.Cleanup(func() { close(w.release) })
	c := sendRawConnection(t, w)
	start := time.Now()
	err := c.sendRaw(t.Context(), []byte(`{}`))
	if !errors.Is(err, errRefusedAnswer) {
		t.Fatalf("sendRaw = %v, want the connection's failure", err)
	}
	if took := time.Since(start); took > writeSettle+2*time.Second {
		t.Fatalf("sendRaw took %v, past writeSettle %v", took, writeSettle)
	}
}
