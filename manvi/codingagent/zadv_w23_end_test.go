package codingagent

import (
	"errors"
	"io"
	"testing"
)

// Attack 4: preferFailure replaces only a plain end of the stream.
func TestAdvW23PreferFailureOnlyReplacesAPlainEnd(t *testing.T) {
	c := advWire(t, 1)
	c.fail(errEndBoom)
	if got := c.preferFailure(io.ErrUnexpectedEOF); !errors.Is(got, io.ErrUnexpectedEOF) || errors.Is(got, errEndBoom) {
		t.Fatalf("io.ErrUnexpectedEOF became %v", got)
	}
	other := errors.New("a real read error")
	if got := c.preferFailure(other); got != other {
		t.Fatalf("a real error became %v", got)
	}
	if got := c.preferFailure(nil); got != nil {
		t.Fatalf("a good frame became %v", got)
	}
	if got := c.preferFailure(io.EOF); !errors.Is(got, errEndBoom) {
		t.Fatalf("EOF stayed %v behind a recorded failure", got)
	}
	// A frame that was fine, behind a recorded failure, is still delivered fine.
	c.frames <- wireRead{value: packet{Method: "m"}}
	p, _, err := c.receiveFrame(t.Context())
	if err != nil || p.Method != "m" {
		t.Fatalf("a good queued frame = %+v, %v", p, err)
	}
	// A plain cancel records no failure, so EOF stays EOF.
	plain := advWire(t, 1)
	plain.cancel()
	if got := plain.preferFailure(io.EOF); got != io.EOF {
		t.Fatalf("EOF behind a plain cancel became %v", got)
	}
}

// Attack 1: the refusal in withdrawAtEnd is unreachable from Next, because Next
// returns everything parked before it reads the wire (codex.go:593, claude.go:624,
// agy.go:614): withdrawAtEnd always meets an empty deferred, and 32 withdrawals
// are far under the bound. Worst case: a preflight that parked its whole budget
// before the stream ended, with the maximum pending.
func TestAdvW23ParkedPreflightPlusMaxPendingIsNeverRefusedAtTheEnd(t *testing.T) {
	s := w18Codex(t)
	var reqs []Request
	for i := 1; i <= maxPendingCallbacks; i++ {
		e, ok, err := s.event(w18CodexAsk(i))
		if err != nil || !ok {
			t.Fatalf("capture %d: ok=%v err=%v", i, ok, err)
		}
		reqs = append(reqs, *e.Request)
	}
	for i := 0; i < maxDrainItems-1; i++ {
		s.wire.frames <- wireRead{value: w18Frame("item/commandExecution/outputDelta", `{"threadId":"thread","turnId":"turn","delta":"x"}`)}
	}
	close(s.wire.frames)
	_ = s.ValidateResponse(reqs[0], Response{Decision: "allow_once"})
	parked := len(s.deferred)
	if parked == 0 {
		t.Fatal("the preflight parked nothing")
	}
	withdrawn := 0
	for {
		e, err := s.Next(t.Context())
		if err != nil {
			if errors.Is(err, errDeferredBacklog) {
				t.Fatalf("refused at the end with %d parked first", parked)
			}
			break
		}
		if e.CancelledID != "" {
			withdrawn++
		}
	}
	if withdrawn != maxPendingCallbacks {
		t.Fatalf("withdrawn %d of %d", withdrawn, maxPendingCallbacks)
	}
}
