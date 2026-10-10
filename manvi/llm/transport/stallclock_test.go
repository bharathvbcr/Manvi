package transport_test

import (
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/llm/transport"
	"github.com/bharathvbcr/Manvi/manvi/llm/transport/transporttest"
)

// TestSlowButLiveStreamIsNotAbandoned. Bytes keep arriving, each gap under the
// limit, total well over it. A bound on total duration would kill this; a bound
// on the gap must not.
//
// The gaps are stated on a driven clock rather than slept through, so that a
// sleep the scheduler overran cannot be mistaken here for a watchdog that fired
// early — the two are the same error to a reader, and that ambiguity is what
// makes the wall-clock version of this test fail under a full-suite run. See
// transporttest.Clock.
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

	clock := transporttest.NewClock()
	start := clock.Now()
	sse := transport.NewSSEWithStall(pr, "[DONE]", limit, clock)
	defer func() { _ = sse.Close() }()
	if n := clock.Scheduled(); n != 1 {
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

// TestALongLineArrivingContinuouslyIsNotAStall. A local model emitting a large
// tool-call payload sends one enormous data: line. Resetting the watchdog only
// when a line *completes* abandons that stream even though bytes are arriving
// the whole time — the harness kills the request precisely when the model is
// doing the most work.
func TestALongLineArrivingContinuouslyIsNotAStall(t *testing.T) {
	const gap = 50 * time.Millisecond
	const limit = 200 * time.Millisecond
	const chunks = 20

	// The gaps are stated rather than slept, for the reason in transporttest.Clock: a
	// sleep the scheduler overran and a watchdog that fired early are the same
	// error here. The reader spends the whole line inside one Next, so the
	// writer is what advances — and it can only ever be one advance ahead,
	// because io.Pipe hands off synchronously and this writer cannot reach its
	// next advance until the reader has taken the bytes before it.
	clock := transporttest.NewClock()
	pr, pw := io.Pipe()
	go func() {
		if _, err := pw.Write([]byte(`data: {"args":"`)); err != nil {
			return
		}
		for i := 0; i < chunks; i++ {
			clock.Advance(gap)
			if _, err := pw.Write([]byte(strings.Repeat("x", 512))); err != nil {
				return
			}
		}
		_, _ = pw.Write([]byte("\"}\n\ndata: [DONE]\n\n"))
		_ = pw.Close()
	}()

	// Every gap is well inside the limit; the line takes five limits to
	// arrive, so a bound on how long the line took would abandon it.
	sse := transport.NewSSEWithStall(pr, "[DONE]", limit, clock)
	defer func() { _ = sse.Close() }()
	if n := clock.Scheduled(); n != 1 {
		t.Fatalf("the stream scheduled %d timers on this test's clock, want 1; the "+
			"watchdog is not running on the clock this test drives", n)
	}

	event, err := sse.Next()
	if err != nil {
		t.Fatalf("a continuously arriving line was abandoned: %v", err)
	}
	if want := chunks * 512; len(event.Data) < want {
		t.Fatalf("data = %d bytes, want at least %d", len(event.Data), want)
	}
	if _, err := sse.Next(); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}
