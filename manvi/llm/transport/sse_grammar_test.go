package transport

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// drainEvents drains a stream into its events, failing the test on any error other
// than the end.
func drainEvents(t *testing.T, s *SSE) []Event {
	t.Helper()
	var events []Event
	for {
		ev, err := s.Next()
		if errors.Is(err, io.EOF) {
			return events
		}
		if err != nil {
			t.Fatalf("Next: %v after %d events", err, len(events))
		}
		events = append(events, Event{Name: ev.Name, Data: append([]byte(nil), ev.Data...)})
		if len(events) > 64 {
			t.Fatal("the stream did not end")
		}
	}
}

// sseSources feeds the same body whole and one byte at a time, because a line
// ending split across two reads is where a CR/LF parser goes wrong.
func sseSources(body string) map[string]io.Reader {
	return map[string]io.Reader{
		"whole":     strings.NewReader(body),
		"bytewise":  iotest.OneByteReader(strings.NewReader(body)),
		"half-read": iotest.HalfReader(strings.NewReader(body)),
	}
}

func eventsOf(t *testing.T, body string, name string) []Event {
	t.Helper()
	for label, src := range sseSources(body) {
		if label != name {
			continue
		}
		return drainEvents(t, NewSSE(io.NopCloser(src), ""))
	}
	t.Fatalf("no source %q", name)
	return nil
}

func wantEvents(t *testing.T, label string, got []Event, want ...Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d events %q, want %d %q", label, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || string(got[i].Data) != string(want[i].Data) {
			t.Fatalf("%s: event %d = {%q %q}, want {%q %q}", label, i, got[i].Name, got[i].Data, want[i].Name, want[i].Data)
		}
	}
}

var sources = []string{"whole", "bytewise", "half-read"}

// TestALoneCarriageReturnEndsALine. The event-stream grammar ends a line at
// CRLF, LF or a bare CR. SSE.Next only ever looked for LF, so a CR-terminated
// stream was one endless line: nothing dispatched until the 32 MiB frame cap.
func TestALoneCarriageReturnEndsALine(t *testing.T) {
	for _, src := range sources {
		wantEvents(t, src+"/cr", eventsOf(t, "data: a\r\rdata: b\r\r", src),
			Event{Data: []byte("a")}, Event{Data: []byte("b")})
		wantEvents(t, src+"/named", eventsOf(t, "event: ping\rdata: {\"type\":\"ping\"}\r\r", src),
			Event{Name: "ping", Data: []byte(`{"type":"ping"}`)})
		wantEvents(t, src+"/multi-line data", eventsOf(t, "data: a\rdata: b\r\r", src),
			Event{Data: []byte("a\nb")})
		wantEvents(t, src+"/mixed endings", eventsOf(t, "event: x\r\ndata: 1\r\r"+"data: 2\n\n"+"data: 3\r\n\r\n", src),
			Event{Name: "x", Data: []byte("1")}, Event{Data: []byte("2")}, Event{Data: []byte("3")})
		wantEvents(t, src+"/trailing cr, no blank line", eventsOf(t, "data: a\r\rdata: b\r", src),
			Event{Data: []byte("a")}, Event{Data: []byte("b")})
	}
}

// TestCRLFStaysOneLineEnding is the converse: reading a bare CR as a line end
// must not turn the CR of every CRLF into a line of its own, which would put a
// blank line after each field and dispatch an event after every line.
func TestCRLFStaysOneLineEnding(t *testing.T) {
	for _, src := range sources {
		wantEvents(t, src+"/crlf", eventsOf(t, "event: x\r\ndata: a\r\n\r\ndata: b\r\ndata: c\r\n\r\n", src),
			Event{Name: "x", Data: []byte("a")}, Event{Data: []byte("b\nc")})
	}
}

// TestALeadingByteOrderMarkIsDropped. A stream is UTF-8 and may open with a
// BOM, which the grammar says to discard. Left in, it glues itself to the first
// field name, so BOM+"event: ping" is a field this parser does not know and
// the first event arrives nameless.
func TestALeadingByteOrderMarkIsDropped(t *testing.T) {
	const bom = "\xEF\xBB\xBF"
	for _, src := range sources {
		wantEvents(t, src+"/named", eventsOf(t, bom+"event: message_start\ndata: {}\n\ndata: b\n\n", src),
			Event{Name: "message_start", Data: []byte("{}")}, Event{Data: []byte("b")})
		wantEvents(t, src+"/data", eventsOf(t, bom+"data: a\n\n", src),
			Event{Data: []byte("a")})
		wantEvents(t, src+"/before a comment", eventsOf(t, bom+": hi\n\ndata: a\n\n", src),
			Event{Data: []byte("a")})
		// Only at the very start: a BOM later in the stream is ordinary text.
		wantEvents(t, src+"/not mid-stream", eventsOf(t, "data: a\n\n"+bom+"data: b\n\n", src),
			Event{Data: []byte("a")})
	}
}

// TestAByteOrderMarkLookalikeIsNotStripped: only the full three-byte mark goes.
func TestAByteOrderMarkLookalikeIsNotStripped(t *testing.T) {
	for _, src := range sources {
		got := eventsOf(t, "\xEF\xBB!data: a\n\n", src)
		if len(got) != 0 {
			t.Fatalf("%s: a partial mark was stripped and the line parsed: %q", src, got)
		}
	}
}

// TestPingEventsReachTheDecoder. Anthropic interleaves `event: ping` frames
// with real ones; they carry no output but are events like any other, and the
// decoder (not the framing) is what ignores them.
func TestPingEventsReachTheDecoder(t *testing.T) {
	body := "event: ping\ndata: {\"type\": \"ping\"}\n\n: keep-alive\n\nevent: content_block_delta\ndata: {}\n\n"
	for _, src := range sources {
		wantEvents(t, src, eventsOf(t, body, src),
			Event{Name: "ping", Data: []byte(`{"type": "ping"}`)},
			Event{Name: "content_block_delta", Data: []byte("{}")})
	}
}

// TestSplitFrameAndFrameFieldsFollowTheSameGrammar. The preflight reads the
// opening frames with these, and the decoder reads the same bytes with
// SSE.Next; two parsers that disagree about where a frame ends or what its
// fields are is how an opening error frame goes unjudged.
func TestSplitFrameAndFrameFieldsFollowTheSameGrammar(t *testing.T) {
	cases := []struct {
		name, in, frame string
		size            int
		ok              bool
	}{
		{"lf", "a\n\nrest", "a", 3, true},
		{"crlf", "a\r\n\r\nrest", "a", 5, true},
		{"cr", "a\r\rrest", "a", 3, true},
		{"cr then crlf", "a\r\r\nrest", "a", 4, true},
		{"crlf then cr", "a\r\n\rrest", "a", 4, true},
		{"mixed", "a\n\r\nrest", "a", 4, true},
		{"two lines", "a\rb\r\rrest", "a\rb", 5, true},
		{"incomplete", "a\r", "", 0, false},
		{"incomplete after one ending", "a\n\r", "", 0, false},
		{"no terminator", "a\nb", "", 0, false},
	}
	for _, tc := range cases {
		frame, size, ok := SplitFrame(tc.in)
		if frame != tc.frame || size != tc.size || ok != tc.ok {
			t.Errorf("%s: SplitFrame(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.name, tc.in, frame, size, ok, tc.frame, tc.size, tc.ok)
		}
	}

	for name, frame := range map[string]string{
		"lf":   "event: error\ndata: {\"a\":1}\ndata: 2",
		"crlf": "event: error\r\ndata: {\"a\":1}\r\ndata: 2",
		"cr":   "event: error\rdata: {\"a\":1}\rdata: 2",
		"bom":  "\xEF\xBB\xBFevent: error\ndata: {\"a\":1}\ndata: 2",
	} {
		n, d := FrameFields(frame)
		if n != "error" || d != "{\"a\":1}\n2" {
			t.Errorf("%s: FrameFields = (%q, %q), want (\"error\", %q)", name, n, d, "{\"a\":1}\n2")
		}
	}
}

// TestPreflightJudgesBOMAndCarriageReturnFramedFrames: the opening frames are
// read with SplitFrame and FrameFields, not by SSE.Next, so each of the
// framings the decoder accepts has to reach the judge too — including a last
// frame whose closing CR is the final byte of the response.
func TestPreflightJudgesBOMAndCarriageReturnFramedFrames(t *testing.T) {
	judge := func(frame string) (bool, *Error) {
		name, _ := FrameFields(frame)
		switch name {
		case "ping":
			return true, nil
		case "error":
			return false, StreamFailure("p", "boom", 0, true)
		}
		return false, nil
	}
	for name, body := range map[string]string{
		"bom":                 "\xEF\xBB\xBFevent: error\ndata: b\n\n",
		"cr":                  "event: ping\rdata: {}\r\revent: error\rdata: b\r\r",
		"cr at end of stream": "event: error\rdata: b\r\r",
		"bom and crlf":        "\xEF\xBB\xBFevent: ping\r\ndata: {}\r\n\r\nevent: error\r\ndata: b\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			resp := &http.Response{Body: io.NopCloser(strings.NewReader(body))}
			defer func() { _ = resp.Body.Close() }()
			accepted, failure := PreflightStream(resp, "p", time.Second, judge)
			if accepted.Body != nil {
				_ = accepted.Body.Close()
			}
			if failure == nil || !failure.Retryable() {
				t.Fatalf("the opening error frame was not judged: %v", failure)
			}
		})
	}
}
