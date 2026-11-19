package codingagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var errEndBoom = errors.New("managed provider ended with a recorded failure")

// A frame stream that ended after the connection recorded why it failed reports
// that failure, not io.EOF, whichever of the ready select cases wins. The loop is
// what makes a select-order bug fail: a closed frames channel and a done context
// are both ready, and the runtime picks one at random.
func TestReceiveFrameReportsARecordedFailureAheadOfEOF(t *testing.T) {
	shapes := map[string]func(c *connection){
		"closed channel": func(c *connection) { close(c.frames) },
		"queued EOF frame": func(c *connection) {
			c.frames <- wireRead{err: io.EOF}
			close(c.frames)
		},
	}
	for name, end := range shapes {
		t.Run(name, func(t *testing.T) {
			for i := 0; i < 500; i++ {
				c := advWire(t, 2)
				c.fail(errEndBoom)
				end(c)
				if _, _, err := c.receiveFrame(t.Context()); !errors.Is(err, errEndBoom) {
					t.Fatalf("run %d: receiveFrame = %v, want the recorded failure", i, err)
				}
			}
		})
	}
}

// read() is the other half: stdout reaching its end while the connection has a
// recorded failure (the stderr cap kills the process, which closes stdout) must
// hand the reader the failure, not an EOF frame, whichever of its own ready
// cases (queue the frame, or see the cancellation) it takes.
func TestReadReportsARecordedFailureAheadOfEOF(t *testing.T) {
	for i := 0; i < 500; i++ {
		c := advWire(t, 1)
		out, in := io.Pipe()
		c.stdout = out
		c.fail(errEndBoom)
		finished := make(chan struct{})
		go func() { c.read(); close(finished) }()
		in.Close()
		// The reader has ended before anyone reads: both the frames channel and the
		// connection's context are ready, the case the race is about.
		<-finished
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		_, _, err := c.receiveFrame(ctx)
		stop()
		if !errors.Is(err, errEndBoom) {
			t.Fatalf("run %d: the reader reported %v, want the recorded failure", i, err)
		}
	}
}

// The preflight drain reads the same channel and owes the same answer.
func TestDrainBacklogReportsARecordedFailureAheadOfEnd(t *testing.T) {
	for _, shape := range []string{"closed", "queued EOF"} {
		t.Run(shape, func(t *testing.T) {
			for i := 0; i < 200; i++ {
				c := advWire(t, 2)
				c.fail(errEndBoom)
				if shape == "closed" {
					close(c.frames)
				} else {
					c.frames <- wireRead{err: io.EOF}
				}
				var queue []int
				var deferred []Event
				err := drainBacklog("Test", c, &queue, &deferred, func(wireRead) int { return 0 },
					func(int) (Event, bool, error) { return Event{}, false, nil })
				if !errors.Is(err, errEndBoom) {
					t.Fatalf("run %d: drain = %v, want the recorded failure", i, err)
				}
			}
		})
	}
}

// ---- connection end and callbacks still awaiting an answer ----------------

type endAdapter struct {
	name string
	next func(context.Context) (Event, error)
	wire *connection
	// offered is the callback ids the adapter holds unanswered, in the order a
	// withdrawal reports them.
	offered []string
	// parked is how many events the adapter holds behind the one it returned.
	parked func() int
}

func endAdapters(t *testing.T, n int) []endAdapter {
	t.Helper()
	ids := func(rs []Request) []string {
		out := make([]string, 0, len(rs))
		for _, r := range rs {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	var out []endAdapter

	agy := advAgyIdle(t)
	agy.emittedTurnStart = true
	var ar []Request
	for i := 0; i < n; i++ {
		ar = append(ar, advAgyOffer(t, agy, fmt.Sprint(100+i)))
	}
	out = append(out, endAdapter{"agy", agy.Next, agy.wire, ids(ar), func() int { return len(agy.deferred) }})

	claude := &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{}, wire: advWire(t, 8)}
	var cr []Request
	for i := 0; i < n; i++ {
		e, ok, err := claude.event([]byte(fmt.Sprintf(`{"type":"control_request","request_id":"r%d","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`, i)))
		if err != nil || !ok || e.Request == nil {
			t.Fatalf("claude capture %d: ok=%v err=%v", i, ok, err)
		}
		cr = append(cr, *e.Request)
	}
	out = append(out, endAdapter{"claude", claude.Next, claude.wire, ids(cr), func() int { return len(claude.deferred) }})

	codex := w18Codex(t)
	var xr []Request
	for i := 1; i <= n; i++ {
		e, ok, err := codex.event(w18CodexAsk(i))
		if err != nil || !ok || e.Request == nil {
			t.Fatalf("codex capture %d: ok=%v err=%v", i, ok, err)
		}
		xr = append(xr, *e.Request)
	}
	out = append(out, endAdapter{"codex", codex.Next, codex.wire, ids(xr), func() int { return len(codex.deferred) }})
	return out
}

// When the connection ends under callbacks still awaiting an answer, every
// adapter says so the same way: each callback is reported withdrawn, in a
// stable order, ahead of the error that ended the connection (the recorded
// failure when there is one, else the stream's end). Agy always did; Claude and
// Codex left the callbacks pending until the run's failure path settled them.
// The count 32 is the per-run pending cap: the largest set one end can report,
// and it must fit the one deferred bound.
func TestConnectionEndWithdrawsPendingCallbacksAheadOfTheError(t *testing.T) {
	for _, n := range []int{1, 3, maxPendingCallbacks} {
		for _, failed := range []bool{true, false} {
			for _, a := range endAdapters(t, n) {
				t.Run(fmt.Sprintf("%s/pending=%d/failed=%v", a.name, n, failed), func(t *testing.T) {
					want := io.EOF
					if failed {
						a.wire.fail(errEndBoom)
						want = errEndBoom
					}
					close(a.wire.frames)
					for i, id := range a.offered {
						e, err := a.next(t.Context())
						if err != nil || e.CancelledID != id {
							t.Fatalf("event %d = %+v, %v; want the withdrawal of %s", i, e, err, id)
						}
						if got := a.parked(); got > maxDeferredBacklog || got != len(a.offered)-1-i {
							t.Fatalf("event %d: %d parked behind it, want %d and at most %d", i, got, len(a.offered)-1-i, maxDeferredBacklog)
						}
					}
					for i := 0; i < 2; i++ {
						if e, err := a.next(t.Context()); !errors.Is(err, want) {
							t.Fatalf("after the withdrawals, read %d = %+v, %v; want %v", i, e, err, want)
						}
					}
				})
			}
		}
	}
}

// A caller's own cancellation leaves the session alive, so nothing is withdrawn
// for it; the callbacks are still there when the connection later ends.
func TestCallerCancelDoesNotWithdrawPendingCallbacks(t *testing.T) {
	for _, a := range endAdapters(t, 2) {
		t.Run(a.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if e, err := a.next(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("a cancelled read = %+v, %v; want the cancel", e, err)
			}
			a.wire.fail(errEndBoom)
			close(a.wire.frames)
			for _, id := range a.offered {
				if e, err := a.next(t.Context()); err != nil || e.CancelledID != id {
					t.Fatalf("after the cancel, %+v, %v; want the withdrawal of %s", e, err, id)
				}
			}
			if _, err := a.next(t.Context()); !errors.Is(err, errEndBoom) {
				t.Fatalf("last read = %v, want the recorded failure", err)
			}
		})
	}
}

// ---- Close during an in-flight write -------------------------------------

// closeAwareWriter is a stdin whose Write blocks until Close, then fails as a
// write to a closed *os.File does.
type closeAwareWriter struct {
	started chan struct{}
	closed  chan struct{}
	wrote   chan struct{}
	once    sync.Once
}

func (w *closeAwareWriter) Write(p []byte) (int, error) {
	close(w.started)
	<-w.closed
	close(w.wrote)
	return 0, os.ErrClosed
}

// Close releases the blocked write and gives it a moment to report, as the
// operating system does when a pipe is closed under a writer: the write's
// failure is then ready before whatever close() does next. A close() that
// cancels only after closing stdin lets sendRaw see the pipe error first.
func (w *closeAwareWriter) Close() error {
	w.once.Do(func() {
		close(w.closed)
		<-w.wrote
		time.Sleep(time.Millisecond)
	})
	return nil
}

// A write in flight when close() ends the connection reports the cancel, not
// the closed pipe, every time: serve classes a cancel "interrupted" and a pipe
// error "failed", and which one a run got used to depend on which ready select
// case won.
func TestCloseDuringAnInFlightWriteReportsTheCancelEveryTime(t *testing.T) {
	for i := 0; i < 300; i++ {
		w := &closeAwareWriter{started: make(chan struct{}), closed: make(chan struct{}), wrote: make(chan struct{})}
		c := advWire(t, 1)
		c.stdin, c.stdout = w, io.NopCloser(strings.NewReader(""))
		c.cmd = &exec.Cmd{}
		c.done = make(chan struct{})
		close(c.done)
		c.groupGone.Store(true)
		sent := make(chan error, 1)
		go func() { sent <- c.sendRaw(t.Context(), []byte(`{}`)) }()
		<-w.started
		if _, err := c.close(); err != nil {
			t.Fatalf("run %d: close: %v", i, err)
		}
		if err := <-sent; !errors.Is(err, context.Canceled) || errors.Is(err, os.ErrClosed) {
			t.Fatalf("run %d: the in-flight write reported %v, want the cancel", i, err)
		}
		if c.failure() != nil {
			t.Fatalf("run %d: a close recorded a failure: %v", i, c.failure())
		}
	}
}

// withdrawAtEnd parks what it reports within the one deferred bound, decided for
// the worst case before it withdraws anything: at the bound it reports
// everything, one past it refuses and the callbacks are still pending (nothing
// was forgotten). The pending cap is below the bound, so Next, which calls it
// with nothing parked, can never be refused.
func TestWithdrawAtEndHoldsToTheDeferredBound(t *testing.T) {
	if maxPendingCallbacks >= maxDeferredBacklog {
		t.Fatalf("maxPendingCallbacks %d must stay below maxDeferredBacklog %d", maxPendingCallbacks, maxDeferredBacklog)
	}
	for _, c := range []struct {
		name    string
		prefill int
		wantErr bool
	}{
		{"nothing parked is never refused", 0, false},
		{"fills the bound exactly", maxDeferredBacklog - maxPendingCallbacks, false},
		{"one past the bound is refused", maxDeferredBacklog - maxPendingCallbacks + 1, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			deferred := make([]Event, c.prefill)
			pending := []Event{{CancelledID: "a"}, {CancelledID: "b"}}
			called := false
			e, ended, err := withdrawAtEnd(t.Context(), &deferred, func() []Event { called = true; return pending })
			if !ended {
				t.Fatal("not ended")
			}
			if c.wantErr {
				if !errors.Is(err, errDeferredBacklog) || called || len(deferred) != c.prefill {
					t.Fatalf("err=%v withdrawn=%v parked=%d: a refusal must forget nothing and park nothing", err, called, len(deferred))
				}
				return
			}
			if err != nil || e.CancelledID != "a" || len(deferred) != c.prefill+1 {
				t.Fatalf("e=%+v err=%v parked=%d", e, err, len(deferred))
			}
		})
	}
}

// The answer that races the turn end, seen from the adapter: ValidateResponse
// passed, then the turn's end arrived before the write, and Respond's own drain
// withdraws the callback. Every adapter answers ErrCallbackWithdrawn and writes
// nothing, which serve reads as a callback gone (managed.go answer), not a fault.
func TestRespondThatRacesTheTurnEndIsWithdrawnForEveryAdapter(t *testing.T) {
	ask := func(i int) []byte {
		return []byte(fmt.Sprintf(`{"type":"control_request","request_id":"r%d","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`, i))
	}
	allow := Response{Decision: "allow_once"}
	t.Run("claude", func(t *testing.T) {
		s := &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{}, wire: advWire(t, 8)}
		e, ok, err := s.event(ask(1))
		if err != nil || !ok {
			t.Fatal(err)
		}
		if err := s.ValidateResponse(*e.Request, allow); err != nil {
			t.Fatal(err)
		}
		s.wire.frames <- wireRead{raw: []byte(`{"type":"result","subtype":"success","session_id":"S","result":"done"}`)}
		if err := s.Respond(t.Context(), *e.Request, allow); !errors.Is(err, ErrCallbackWithdrawn) {
			t.Fatalf("Respond = %v, want ErrCallbackWithdrawn", err)
		}
	})
	t.Run("codex", func(t *testing.T) {
		s := w18Codex(t)
		e, ok, err := s.event(w18CodexAsk(1))
		if err != nil || !ok {
			t.Fatal(err)
		}
		if err := s.ValidateResponse(*e.Request, allow); err != nil {
			t.Fatal(err)
		}
		s.wire.frames <- wireRead{value: w18CodexEnd}
		if err := s.Respond(t.Context(), *e.Request, allow); !errors.Is(err, ErrCallbackWithdrawn) {
			t.Fatalf("Respond = %v, want ErrCallbackWithdrawn", err)
		}
	})
	t.Run("agy", func(t *testing.T) {
		s := advAgyIdle(t)
		r := advAgyOffer(t, s, "1")
		if err := s.ValidateResponse(r, allow); err != nil {
			t.Fatal(err)
		}
		s.wire.frames <- wireRead{value: advAgyEnd}
		if err := s.Respond(t.Context(), r, allow); !errors.Is(err, ErrCallbackWithdrawn) {
			t.Fatalf("Respond = %v, want ErrCallbackWithdrawn", err)
		}
	})
}
