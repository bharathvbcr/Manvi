package transport

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestOneEventIsBoundedAcrossItsDataLines.
//
// MaxFrameBytes bounds a *line*. An event is every data: line up to the blank
// line that dispatches it, and Next appended each one onto the same slice with
// nothing watching the total — so a server sending well-formed 64KiB lines and
// never a blank line grew that slice for as long as it kept talking. Measured
// before the bound existed: 134,219,775 bytes in one event, 175 MiB of heap,
// four times MaxFrameBytes, and still climbing when the generator stopped.
//
// Nothing above catches it. payloadReader re-arms the stall watchdog on every
// byte that arrives, so the stream looks healthy, and every adapter's decode
// cap is consulted only after a frame decodes — which never happens while the
// event is still being assembled.
//
// The body here ends at EOF rather than running forever, so the pre-fix
// behaviour is a concrete oversized event rather than a hang: without the
// bound Next returns that whole accumulation as one successful event.
func TestOneEventIsBoundedAcrossItsDataLines(t *testing.T) {
	const lineBytes = 64 << 10
	payload := strings.Repeat("a", lineBytes)

	pr, pw := io.Pipe()
	go func() {
		// A megabyte past the bound, so the assertion is about the bound and
		// not about where the generator happened to stop. No blank line is
		// ever written: this is one event, by the wire's own framing rules.
		for written := 0; written < MaxEventBytes+(1<<20); written += lineBytes + 1 {
			if _, err := io.WriteString(pw, "data: "+payload+"\n"); err != nil {
				return
			}
		}
		pw.Close()
	}()

	s := NewSSE(pr, "")
	// Closes the pipe, so the generator above unblocks and exits rather than
	// parking on a write nobody will read once the bound fires.
	defer s.Close()

	event, err := s.Next()
	var oversized *ErrOversizedEvent
	if !errors.As(err, &oversized) {
		t.Fatalf("Next() returned a %d-byte event and err %v; want *ErrOversizedEvent — "+
			"an event that never ends was buffered without bound", len(event.Data), err)
	}
	if oversized.Limit != MaxEventBytes {
		t.Errorf("reported limit %d, want %d", oversized.Limit, MaxEventBytes)
	}

	// The failure is terminal, like every other error this reader reports: a
	// caller that asks again must not be told the stream is fine.
	if _, err := s.Next(); !errors.As(err, &oversized) {
		t.Errorf("a second Next() returned %v; the oversized-event failure did not stick", err)
	}
}

// TestAMultiLineEventUnderTheBoundStillAssembles is the control. The bound must
// not change what a legitimate multi-line event decodes to — data: lines are
// joined with a newline between them and nothing else.
func TestAMultiLineEventUnderTheBoundStillAssembles(t *testing.T) {
	body := "data: one\ndata: two\ndata: three\n\n"
	s := NewSSE(io.NopCloser(strings.NewReader(body)), "")
	defer s.Close()

	event, err := s.Next()
	if err != nil {
		t.Fatalf("Next() = %v, want a decoded event", err)
	}
	if got, want := string(event.Data), "one\ntwo\nthree"; got != want {
		t.Errorf("event data = %q, want %q", got, want)
	}
}
