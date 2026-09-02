package transport

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// blockingBody yields a first frame, then blocks forever, then unblocks on
// Close — which is exactly how a wedged server behaves to a reader.
type blockingBody struct {
	first  string
	sent   bool
	closed chan struct{}
}

func newBlockingBody(first string) *blockingBody {
	return &blockingBody{first: first, closed: make(chan struct{})}
}

func (b *blockingBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		n := copy(p, b.first)
		return n, nil
	}
	<-b.closed
	return 0, errors.New("use of closed network connection")
}

func (b *blockingBody) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

func TestStreamThatStopsSendingIsAbandonedAndSaysWhy(t *testing.T) {
	body := newBlockingBody("data: {\"a\":1}\n\n")
	sse := NewSSEWithStall(body, "[DONE]", 150*time.Millisecond, nil)
	defer func() { _ = sse.Close() }()

	if _, err := sse.Next(); err != nil {
		t.Fatalf("the first frame should arrive normally: %v", err)
	}

	start := time.Now()
	_, err := sse.Next()
	elapsed := time.Since(start)

	var stalled *ErrStalled
	if !errors.As(err, &stalled) {
		t.Fatalf("err = %v, want ErrStalled", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the watchdog took %s to fire", elapsed)
	}
	// The message must send an operator to the right place. It used to claim
	// the server had stopped, which this layer cannot determine — silence from
	// a cold prefill and silence from a wedged process are the same bytes — so
	// what is required now is that it names the silence and does not assert a
	// cause it has no evidence for.
	if !strings.Contains(err.Error(), "no output") {
		t.Errorf("the stall message does not name the silence: %v", err)
	}
	if strings.Contains(err.Error(), "it is a stopped one") {
		t.Errorf("the stall message asserts a cause it cannot know: %v", err)
	}
	if !stalled.Timeout() {
		t.Error("a stall should classify as a timeout")
	}
}

// TestSlowButLiveStreamIsNotAbandoned. Bytes keep arriving, each gap under the
// limit, total well over it. A bound on total duration would kill this; a bound
// on the gap must not.
//
// The gaps are stated on a driven clock rather than slept through, so that a
// sleep the scheduler overran cannot be mistaken here for a watchdog that fired
// early — the two are the same error to a reader, and that ambiguity is what
// makes the wall-clock version of this test fail under a full-suite run. See
// manualClock.
func TestSlowButLiveStreamIsNotAbandoned(t *testing.T) {
	const gap = 60 * time.Millisecond
	const limit = 200 * time.Millisecond
	const want = 6

	pr, pw := io.Pipe()
	go func() {
		for i := 0; i < want; i++ {
			_, _ = pw.Write([]byte("data: {\"i\":1}\n\n"))
		}
		_, _ = pw.Write([]byte("data: [DONE]\n\n"))
		_ = pw.Close()
	}()

	clock := newManualClock()
	start := clock.Now()
	sse := NewSSEWithStall(pr, "[DONE]", limit, clock)
	defer func() { _ = sse.Close() }()
	if n := clock.scheduled(); n != 1 {
		t.Fatalf("the stream scheduled %d timers on this test's clock, want 1; the "+
			"watchdog is not running on the clock this test drives", n)
	}

	frames := 0
	for {
		_, err := sse.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("a slow but live stream was abandoned after %d frames at %s of "+
				"stated stream time, with no gap longer than %s: %v",
				frames, clock.Now().Sub(start), gap, err)
		}
		frames++
		// Between reads, so each advance lands in a gap the watchdog is being
		// asked to tolerate. A watchdog bounding the whole stream would have
		// its deadline crossed here.
		clock.Advance(gap)
	}
	if frames != want {
		t.Fatalf("frames = %d, want %d", frames, want)
	}
	if elapsed := clock.Now().Sub(start); elapsed <= limit {
		t.Fatalf("the stream ran for %s against a %s limit; it has to outlast the limit "+
			"in total or it is not testing what the name says", elapsed, limit)
	}
}

func TestStallWatchdogIsOptional(t *testing.T) {
	body := newBlockingBody("data: {\"a\":1}\n\n")
	defer func() { _ = body.Close() }()
	sse := NewSSEWithStall(body, "[DONE]", 0, nil)
	if sse.watchdog != nil {
		t.Fatal("a non-positive limit must not arm the watchdog")
	}
}
