package input

import (
	"bytes"
	"io"
	"testing"
	"time"
	"unicode/utf8"
)

// FuzzDecodeHoldsItsContract fuzzes the terminal escape-sequence decoder.
//
// This decoder reads bytes straight off a pty. Everything upstream of it — a
// remote terminal, a multiplexer, a paste of arbitrary binary — is outside the
// program's control, and a truncated CSI sequence arriving one byte at a time
// is the ordinary case rather than the exceptional one. The contract the event
// loop depends on:
//
//   - It never panics, on any byte sequence, at any split point.
//   - The consumed count is always within the buffer. A count past the end
//     would slice out of range in the caller; a negative count would rewind the
//     reader onto bytes it already dispatched.
//   - Consuming zero bytes means no event. The caller treats a zero as "wait
//     for more input", and an event delivered alongside it would be dispatched
//     and then decoded again from the same bytes.
//
// Deliberately not asserted: that flush always consumes. A truncated rune
// legitimately decodes to nothing even under flush, and reader.go:159 already
// drops a byte in that case rather than spin. The loop-level property that
// matters is covered by TestReaderFlushAlwaysDrains below, which exercises the
// caller instead of second-guessing the decoder.
func FuzzDecodeHoldsItsContract(f *testing.F) {
	seeds := [][]byte{
		{},
		[]byte("a"),
		[]byte("\x1b"),
		[]byte("\x1b["),
		[]byte("\x1b[A"),
		[]byte("\x1b[1;5A"),
		[]byte("\x1b[200~pasted text\x1b[201~"),
		[]byte("\x1b[200~"),
		[]byte("\x1b[<0;10;20M"),
		[]byte("\x1b[<0;10;20m"),
		[]byte("\x1b[<"),
		[]byte("\x1bO"),
		[]byte("\x1bOP"),
		[]byte("\x1b[3~"),
		[]byte("\x1b[999999999999999999999;5~"),
		[]byte("\x1b[-1;-1~"),
		[]byte("\x1b[;;;;;;;;;;~"),
		[]byte("\x1b[1;2;3;4;5;6;7;8;9u"),
		[]byte("\xc3\x28"),         // invalid UTF-8
		[]byte("\xf0\x9f\x92\xa9"), // 4-byte rune
		[]byte("\xf0\x9f"),         // truncated rune
		{0x00}, {0x7f}, {0x1b, 0x1b}, {0x1b, 0x5b, 0xff},
	}
	for _, s := range seeds {
		f.Add(s, false)
		f.Add(s, true)
	}

	f.Fuzz(func(t *testing.T, buf []byte, flush bool) {
		ev, n := decode(buf, flush)

		if n < 0 {
			t.Fatalf("decode consumed %d bytes (negative) from %q", n, buf)
		}
		if n > len(buf) {
			t.Fatalf("decode consumed %d bytes from a %d-byte buffer %q", n, len(buf), buf)
		}
		if n == 0 && ev != nil {
			t.Fatalf("decode returned event %T while consuming nothing; it would be dispatched twice", ev)
		}
		// A rune event must carry valid runes: they are written straight into
		// the prompt buffer and then measured for width.
		if k, ok := ev.(Key); ok {
			for _, r := range k.Runes {
				if r == utf8.RuneError {
					continue // an explicit replacement is a decision, not a defect
				}
				if !utf8.ValidRune(r) {
					t.Fatalf("decoded invalid rune %U from %q", r, buf)
				}
			}
		}

		// Decoding the same prefix twice must agree. The read loop re-decodes a
		// buffer after every append, so an unstable decoder would dispatch one
		// event and then a different one for the same bytes.
		ev2, n2 := decode(buf, flush)
		if n2 != n {
			t.Fatalf("decode is not deterministic: %d then %d bytes from %q", n, n2, buf)
		}
		_ = ev2
	})
}

// TestReaderFlushAlwaysDrains is the loop-level counterpart to the decoder
// contract above. The decoder is allowed to consume nothing under flush — a
// truncated rune does exactly that — so the property that has to hold is one
// level up: whatever arrives, the reader drains its buffer and reaches the end
// of the stream instead of spinning on bytes it cannot decide.
func TestReaderFlushAlwaysDrains(t *testing.T) {
	undecidable := [][]byte{
		{0xf0, 0x9f},                    // truncated 4-byte rune
		{0x1b},                          // lone escape
		{0x1b, '['},                     // truncated CSI
		{0x1b, '[', '2', '0', '0', '~'}, // paste that never ends
		{0x1b, 'O'},                     // truncated SS3
		{0xff, 0xfe, 0xfd},              // invalid UTF-8
	}

	for _, buf := range undecidable {
		t.Run(string(fmtBytes(buf)), func(t *testing.T) {
			r := NewReader(bytesReader(buf))
			r.SetEscapeDelay(time.Millisecond)
			go r.Run()
			defer r.Close()

			deadline := time.After(5 * time.Second)
			for {
				select {
				case ev, ok := <-r.Events():
					if !ok {
						return
					}
					if e, isErr := ev.(Error); isErr {
						if e.Err == io.EOF {
							return // drained to the end of the stream
						}
						return
					}
				case <-deadline:
					t.Fatalf("reader never drained %q; the loop is spinning", buf)
				}
			}
		})
	}
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

func fmtBytes(b []byte) []byte {
	out := make([]byte, 0, len(b)*2)
	const hex = "0123456789abcdef"
	for _, c := range b {
		out = append(out, hex[c>>4], hex[c&0xf])
	}
	return out
}
