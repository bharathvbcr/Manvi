package input

import (
	"io"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// pasteHarness feeds a Reader through a pipe so a test controls the gaps between
// reads, and collects what came out.
type pasteHarness struct {
	t  *testing.T
	pw *io.PipeWriter
	r  *Reader
}

func newPasteHarness(t *testing.T, configure func(*Reader)) *pasteHarness {
	t.Helper()
	pr, pw := io.Pipe()
	r := NewReader(pr)
	if configure != nil {
		configure(r)
	}
	go r.Run()
	t.Cleanup(func() { _ = pw.Close(); r.Close() })
	return &pasteHarness{t: t, pw: pw, r: r}
}

func (h *pasteHarness) write(s string) {
	h.t.Helper()
	if _, err := h.pw.Write([]byte(s)); err != nil {
		h.t.Fatalf("write: %v", err)
	}
}

// next returns the next event or fails after a bound.
func (h *pasteHarness) next() Event {
	h.t.Helper()
	select {
	case ev, ok := <-h.r.Events():
		if !ok {
			h.t.Fatal("event stream closed early")
		}
		return ev
	case <-time.After(10 * time.Second):
		h.t.Fatal("no event arrived")
		return nil
	}
}

// drainPaste reads events until the paste text has reached want, failing on any
// event that is not a Paste. It returns how many Paste events carried it.
func (h *pasteHarness) drainPaste(want string) int {
	h.t.Helper()
	var got strings.Builder
	pieces := 0
	for got.Len() < len(want) {
		ev := h.next()
		p, ok := ev.(Paste)
		if !ok {
			h.t.Fatalf("got %#v after paste text %q, want more Paste text (a key here means the paste tail was typed)", ev, got.String())
		}
		got.WriteString(p.Text)
		pieces++
	}
	if got.String() != want {
		h.t.Fatalf("paste text = %q, want %q", got.String(), want)
	}
	return pieces
}

func (h *pasteHarness) expectKeys(runes string) {
	h.t.Helper()
	for _, want := range runes {
		ev := h.next()
		k, ok := ev.(Key)
		if !ok || len(k.Runes) != 1 || k.Runes[0] != want {
			h.t.Fatalf("got %#v, want key %q", ev, want)
		}
	}
}

// Criterion 1: a gap longer than the escape grace inside a paste does not end it.
func TestPasteAcrossAGapLongerThanTheEscapeDelayIsOnePaste(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + "first line")
	time.Sleep(3 * DefaultEscapeDelay)
	h.write("\nsecond line\x1b[201~")
	ev := h.next()
	p, ok := ev.(Paste)
	if !ok {
		t.Fatalf("got %#v, want one Paste (a KeyEnter here is the newline typed as Enter)", ev)
	}
	if p.Text != "first line\nsecond line" {
		t.Fatalf("paste text = %q", p.Text)
	}
}

// Criterion 2: the end marker closes the paste and keys are keys again.
func TestPasteEndMarkerReturnsToKeys(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + "a")
	time.Sleep(2 * DefaultEscapeDelay)
	h.write("b" + pasteEnd + "xy\r")
	if p, ok := h.next().(Paste); !ok || p.Text != "ab" {
		t.Fatalf("want Paste ab, got %#v", p)
	}
	h.expectKeys("xy")
	if k, ok := h.next().(Key); !ok || k.Type != KeyEnter {
		t.Fatalf("a carriage return after the paste must be Enter, got %#v", k)
	}
}

// Criterion 3a: a paste that never closes is delivered at the paste deadline,
// not before, and the rest of it is still paste text.
func TestUnclosedPasteIsFlushedAtTheDeadlineAndItsTailIsNotTyped(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(300 * time.Millisecond)
	})
	start := time.Now()
	h.write(pasteStart + "head")
	if p, ok := h.next().(Paste); !ok || p.Text != "head" {
		t.Fatalf("want the head delivered at the deadline, got %#v", p)
	}
	if waited := time.Since(start); waited < 250*time.Millisecond {
		t.Fatalf("the paste was delivered after %v, before its deadline: the escape grace was applied", waited)
	}
	h.write("\ntail\r")
	h.drainPaste("\ntail\n")
	h.write("\x1b[201")
	h.write("~z")
	h.expectKeys("z")
}

// The deadline is measured from the start of the paste: bytes arriving inside it
// do not push it back.
func TestPasteDeadlineIsNotExtendedByProgress(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(300 * time.Millisecond)
	})
	start := time.Now()
	h.write(pasteStart)
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(40 * time.Millisecond):
				if _, err := h.pw.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	if _, ok := h.next().(Paste); !ok {
		t.Fatal("want a Paste at the deadline")
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("a trickle held the deadline off for %v", waited)
	}
}

// Criterion 3b: the byte limit delivers at the limit, pending stays bounded, and
// the rest is not typed.
func TestPasteOverTheByteLimitIsFlushedAndBounded(t *testing.T) {
	const limit, chunk = 4096, 1000
	pr, pw := io.Pipe()
	r := NewReader(pr)
	r.SetMaxPasteBytes(limit)
	go r.Run()
	t.Cleanup(r.Close)

	body := strings.Repeat("ab\n", 5000) // far past the limit, with newlines
	go func() {
		payload := pasteStart + body + pasteEnd + "q"
		for i := 0; i < len(payload); i += chunk {
			if _, err := pw.Write([]byte(payload[i:min(i+chunk, len(payload))])); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()

	var got strings.Builder
	pieces := 0
	var sawKey bool
	for ev := range r.Events() {
		switch e := ev.(type) {
		case Paste:
			got.WriteString(e.Text)
			pieces++
		case Key:
			if string(e.Runes) != "q" || got.Len() != len(body) {
				t.Fatalf("key %#v after %d of %d paste bytes: the tail was typed", e, got.Len(), len(body))
			}
			sawKey = true
		}
	}
	if got.String() != body {
		t.Fatalf("paste text differs: got %d bytes, want %d", got.Len(), len(body))
	}
	if pieces < 2 {
		t.Fatalf("a paste over the limit arrived as %d piece(s)", pieces)
	}
	if !sawKey {
		t.Fatal("the key after the end marker was lost")
	}
	// Memory: a decode pass runs after every read, so what is buffered never
	// exceeds the limit plus the one read that crossed it.
	if r.peakPending > limit+chunk+len(pasteStart) {
		t.Fatalf("pending grew to %d bytes with a %d limit and %d-byte reads", r.peakPending, limit, chunk)
	}
}

// A paste cut mid-character by a forced flush does not corrupt the character.
func TestForcedFlushDoesNotSplitARuneOrTheEndMarker(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(100 * time.Millisecond)
	})
	snow := "☃"
	h.write(pasteStart + "a" + snow[:2])
	if p, ok := h.next().(Paste); !ok || p.Text != "a" {
		t.Fatalf("want Paste a with the partial rune held back, got %#v", p)
	}
	h.write(snow[2:] + "\x1b[20")
	h.drainPaste(snow)
	h.write("1~k")
	h.expectKeys("k")
}

// Criterion 4: the escape grace is unchanged for what is not a paste.
func TestLoneEscapeStillResolvesAtDefaultEscapeDelay(t *testing.T) {
	h := newPasteHarness(t, nil)
	start := time.Now()
	h.write("\x1b")
	k, ok := h.next().(Key)
	if !ok || k.Type != KeyEscape {
		t.Fatalf("got %#v, want KeyEscape", k)
	}
	if waited := time.Since(start); waited < DefaultEscapeDelay/2 || waited > DefaultPasteDeadline/2 {
		t.Fatalf("lone escape resolved after %v, want about %v", waited, DefaultEscapeDelay)
	}
}

func TestIncompleteNonPasteCSIStillFlushesAtDefaultEscapeDelay(t *testing.T) {
	h := newPasteHarness(t, nil)
	start := time.Now()
	h.write("\x1b[1;")
	if k, ok := h.next().(Key); !ok || k.Type != KeyEscape {
		t.Fatalf("want the abandoned CSI to flush as Escape, got %#v", k)
	}
	if waited := time.Since(start); waited > DefaultPasteDeadline/2 {
		t.Fatalf("incomplete CSI waited %v: the paste deadline was applied to it", waited)
	}
}

// FuzzPasteReadSplits: a valid paste, with keys around it, decodes to the same
// events however the stream is cut into reads.
func FuzzPasteReadSplits(f *testing.F) {
	f.Add("hello\nworld", int64(1), uint8(3))
	f.Add("", int64(2), uint8(1))
	f.Add("☃ \x1b[201 \x1b[20 end", int64(3), uint8(5))
	f.Add(strings.Repeat("0123456789\n", 40), int64(4), uint8(64))
	f.Fuzz(func(t *testing.T, body string, seed int64, maxChunk uint8) {
		if strings.Contains(body, pasteEnd) {
			t.Skip("the body would close the paste early")
		}
		stream := "a" + pasteStart + body + pasteEnd + "b"
		want := collectAll(t, []string{stream})

		rng := rand.New(rand.NewSource(seed))
		var chunks []string
		for rest := stream; len(rest) > 0; {
			n := min(1+rng.Intn(int(maxChunk)+1), len(rest))
			chunks = append(chunks, rest[:n])
			rest = rest[n:]
		}
		got := collectAll(t, chunks)
		if got != want {
			t.Fatalf("split %q decoded as %s, unsplit as %s", chunks, got, want)
		}
	})
}

// collectAll feeds the chunks with no gaps and returns the events, with Paste
// pieces merged, as a string.
func collectAll(t *testing.T, chunks []string) string {
	t.Helper()
	pr, pw := io.Pipe()
	r := NewReader(pr)
	// The fuzzer runs many workers; a scheduling stall of 50 ms between two
	// reads would fire the real escape timer and make the split observable,
	// which is the timing behaviour tested elsewhere, not this property.
	r.SetEscapeDelay(time.Minute)
	go r.Run()
	defer r.Close()
	go func() {
		for _, c := range chunks {
			if _, err := pw.Write([]byte(c)); err != nil {
				return
			}
		}
		_ = pw.Close()
	}()
	var out strings.Builder
	inPaste := false
	for ev := range r.Events() {
		switch e := ev.(type) {
		case Paste:
			if !inPaste {
				out.WriteString("[paste:")
				inPaste = true
			}
			out.WriteString(e.Text)
		case Key:
			if inPaste {
				out.WriteString("]")
				inPaste = false
			}
			out.WriteString("<" + e.String() + ">")
		}
	}
	if inPaste {
		out.WriteString("]")
	}
	return out.String()
}
