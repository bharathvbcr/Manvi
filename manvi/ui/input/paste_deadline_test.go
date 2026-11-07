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
// not before, and paste mode ends there: what follows is keys, and a later stray
// end marker is a no-op rather than a key.
func TestUnclosedPasteIsFlushedAtTheDeadlineThenKeysAreKeys(t *testing.T) {
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
	h.write("ok" + pasteEnd + "z")
	h.expectKeys("ok")
	h.expectKeys("z") // the stray end marker between them produced nothing
}

// The deadline is measured from the start of the paste: bytes arriving inside
// it do not push it back. A byte every 30 ms is far inside a 300 ms deadline, so
// a deadline restarted by each chunk never fires, and one that fires early is the
// escape grace applied to a paste; both are caught by bounding when it fires.
func TestPasteDeadlineIsNotExtendedByProgress(t *testing.T) {
	const deadline = 300 * time.Millisecond
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(deadline)
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
			case <-time.After(30 * time.Millisecond):
				if _, err := h.pw.Write([]byte("x")); err != nil {
					return
				}
			}
		}
	}()
	ev, ok := advRecv(h, 3*deadline)
	if !ok {
		t.Fatalf("no Paste within %v: a trickle of bytes held the deadline off", 3*deadline)
	}
	if _, isPaste := ev.(Paste); !isPaste {
		t.Fatalf("got %#v, want a Paste", ev)
	}
	waited := time.Since(start)
	if waited < deadline*3/4 || waited > deadline*2 {
		t.Fatalf("the paste was delivered after %v; want about the %v deadline from its start", waited, deadline)
	}
}

// A Ctrl+C byte is never paste text. It ends an open paste at once: the text
// before it is delivered, then the key.
func TestCtrlCInsideAnOpenPasteIsAKeyAtOnce(t *testing.T) {
	h := newPasteHarness(t, nil) // default 5 s deadline: nothing here may wait for it
	h.write(pasteStart + "abc\ndef\x03")
	if p, ok := h.next().(Paste); !ok || p.Text != "abc\ndef" {
		t.Fatalf("want the text before ^C as a Paste, got %#v", p)
	}
	if ev := h.next(); !advIsCtrl(ev, 'c') {
		t.Fatalf("got %#v, want Ctrl+C", ev)
	}
	// Paste mode is over: a carriage return is Enter again and a stray marker is a no-op.
	h.write("\r" + pasteEnd + "z")
	if k, ok := h.next().(Key); !ok || k.Type != KeyEnter {
		t.Fatalf("after ^C a carriage return must be Enter, got %#v", k)
	}
	h.expectKeys("z")
}

func TestCtrlCRightAfterPasteStartEmitsNoEmptyPaste(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + "\x03")
	if ev := h.next(); !advIsCtrl(ev, 'c') {
		t.Fatalf("got %#v, want Ctrl+C first, with no empty Paste before it", ev)
	}
}

// The invariant that makes a lost or injected ESC[200~ survivable: Ctrl+C works
// immediately, and every key works once the paste deadline has passed.
func TestInjectedPasteStartNeverLocksTheKeyboard(t *testing.T) {
	const deadline = 200 * time.Millisecond
	configure := func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(deadline)
	}
	h := newPasteHarness(t, configure)
	h.write(pasteStart)
	h.write("\x03")
	if ev, ok := advRecv(h, deadline/2); !ok || !advIsCtrl(ev, 'c') {
		t.Fatalf("Ctrl+C inside an injected paste: got %#v (ok=%v), want Ctrl+C before the deadline", ev, ok)
	}

	h = newPasteHarness(t, configure)
	h.write(pasteStart)
	time.Sleep(deadline + 100*time.Millisecond)
	h.write("hi\x04\x1b")
	h.expectKeys("hi")
	if ev := h.next(); !advIsCtrl(ev, 'd') {
		t.Fatalf("got %#v, want Ctrl+D", ev)
	}
	if k, ok := h.next().(Key); !ok || k.Type != KeyEscape {
		t.Fatalf("want a lone Escape after the deadline, got %#v", k)
	}
}

// A paste that reaches the byte limit is a live stream, so its end marker is
// still expected; but when input goes quiet the continuation ends and keys are
// keys, with a stray end marker a no-op.
func TestContinuationEndsWhenInputGoesIdle(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetMaxPasteBytes(64)
		r.SetPasteIdle(150 * time.Millisecond)
	})
	h.write(pasteStart + strings.Repeat("p", 100))
	h.drainPaste(strings.Repeat("p", 100))
	time.Sleep(400 * time.Millisecond)
	h.write("k" + pasteEnd + "j")
	h.expectKeys("kj")
}

// Inside a continuation, Ctrl+C is a key at once and ends it.
func TestCtrlCEndsAContinuation(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetMaxPasteBytes(64)
		r.SetPasteIdle(time.Minute)
	})
	h.write(pasteStart + strings.Repeat("p", 100))
	h.drainPaste(strings.Repeat("p", 100))
	h.write("more\x03")
	h.drainPaste("more")
	if ev := h.next(); !advIsCtrl(ev, 'c') {
		t.Fatalf("got %#v, want Ctrl+C", ev)
	}
	h.write("q")
	h.expectKeys("q")
}

// A second paste opening right behind the first gets its own deadline, from its
// own start, not what remained of the first one's.
func TestSecondPasteGetsItsOwnDeadline(t *testing.T) {
	const deadline = 400 * time.Millisecond
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(deadline)
	})
	h.write(pasteStart + "a")
	time.Sleep(250 * time.Millisecond)
	second := time.Now()
	h.write(pasteEnd + pasteStart + "b")
	for {
		ev, ok := advRecv(h, 3*time.Second)
		if !ok {
			t.Fatal("no event for the second paste")
		}
		p, isPaste := ev.(Paste)
		if !isPaste {
			t.Fatalf("got %#v", ev)
		}
		if p.Text == "b" {
			if waited := time.Since(second); waited < deadline*3/4 {
				t.Fatalf("paste 2 was flushed %v after it opened; its deadline is %v", waited, deadline)
			}
			return
		}
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

// A continuation piece cut at the byte limit does not split a character or the
// end marker: those bytes wait for the next read.
func TestLimitFlushDoesNotSplitARuneOrTheEndMarker(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetMaxPasteBytes(16)
		r.SetPasteIdle(time.Minute)
	})
	snow := "\u2603"
	filler := strings.Repeat("a", 14)
	h.write(pasteStart + filler + snow[:2])
	if p, ok := h.next().(Paste); !ok || p.Text != filler {
		t.Fatalf("want the filler with the partial rune held back, got %#v", p)
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
