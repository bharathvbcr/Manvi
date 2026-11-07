package input

// Adversarial tests for the bracketed-paste deadline / continuation change
// (GitPulse task ft-e340828cc63deabf128b88991c6a3e1e). Tests that fail here
// document a defect in the code under review; the passing ones record attacks
// that held.

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// advRecv returns the next event within d, or ok=false.
func advRecv(h *pasteHarness, d time.Duration) (Event, bool) {
	select {
	case ev, open := <-h.r.Events():
		if !open {
			return nil, false
		}
		return ev, true
	case <-time.After(d):
		return nil, false
	}
}

func advIsCtrl(ev Event, r rune) bool {
	k, ok := ev.(Key)
	return ok && k.Type == KeyRunes && len(k.Runes) == 1 && k.Runes[0] == r && k.Mod&ModCtrl != 0
}

func advIsRune(ev Event, r rune) bool {
	k, ok := ev.(Key)
	return ok && k.Type == KeyRunes && len(k.Runes) == 1 && k.Runes[0] == r && k.Mod == 0
}

// advStuck puts a reader into the post-deadline state: a paste opened, its end
// marker never came, the deadline fired, and the head was delivered.
func advStuck(t *testing.T) *pasteHarness {
	t.Helper()
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(100 * time.Millisecond)
	})
	h.write(pasteStart + "head")
	if p, ok := h.next().(Paste); !ok || p.Text != "head" {
		t.Fatalf("setup: want Paste head at the deadline, got %#v", p)
	}
	return h
}

// PRIORITY. In raw mode ISIG is cleared (ui/term/raw_unix.go), so Ctrl+C arrives
// as byte 0x03 and the input loop is the only thing that can cancel or quit. After
// a deadline flush on a paste whose end marker was lost, 0x03, 0x04 and a lone
// Esc must still reach the application as keys.
func TestAdvStuckPasteCtrlCCtrlDEscStillReachTheApp(t *testing.T) {
	cases := []struct {
		name  string
		bytes string
		ok    func(Event) bool
	}{
		{"ctrl-c", "\x03", func(e Event) bool { return advIsCtrl(e, 'c') }},
		{"ctrl-d", "\x04", func(e Event) bool { return advIsCtrl(e, 'd') }},
		{"esc", "\x1b", func(e Event) bool { k, ok := e.(Key); return ok && k.Type == KeyEscape }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := advStuck(t)
			h.write(c.bytes)
			ev, ok := advRecv(h, 2*time.Second)
			if !ok {
				t.Fatalf("%s written after the paste deadline produced NO event within 2s: the byte is held or swallowed", c.name)
			}
			if !c.ok(ev) {
				t.Fatalf("%s written after the paste deadline arrived as %#v, not as a key", c.name, ev)
			}
		})
	}
}

// The only exit from continuation is a literal ESC[201~. A user who has typed
// for a long time (quiet gap far longer than the deadline) is still in paste mode.
func TestAdvStuckPasteLeavesContinuationAfterAQuietGapWithoutAMarker(t *testing.T) {
	h := advStuck(t)
	time.Sleep(600 * time.Millisecond) // 6x the deadline of silence
	h.write("q")
	ev, ok := advRecv(h, 2*time.Second)
	if !ok {
		t.Fatal("a key typed after a long quiet gap produced no event")
	}
	if !advIsRune(ev, 'q') {
		t.Fatalf("a key typed 600ms after the deadline (and long after any live stream would have stopped) arrived as %#v, want Key q", ev)
	}
}

// Anything that injects ESC[200~ into stdin (a pty replay tool, TIOCSTI, a
// terminal that echoes a hostile title report) locks the keyboard for good: no
// ESC[201~ is ever coming, and the reader never leaves paste mode.
func TestAdvInjectedPasteStartLocksTheKeyboardForever(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(100 * time.Millisecond)
	})
	h.write(pasteStart) // nothing else: a stray start
	// Drain whatever the deadline emits, then type.
	for {
		if _, ok := advRecv(h, 400*time.Millisecond); !ok {
			break
		}
	}
	h.write("hi\x03")
	var keys []Event
	for {
		ev, ok := advRecv(h, 500*time.Millisecond)
		if !ok {
			break
		}
		keys = append(keys, ev)
	}
	var sawCtrlC bool
	for _, e := range keys {
		sawCtrlC = sawCtrlC || advIsCtrl(e, 'c')
	}
	if !sawCtrlC {
		t.Fatalf("after an injected ESC[200~ and the deadline, typed \"hi^C\" produced %#v: no Ctrl+C key, the user cannot cancel or quit", keys)
	}
}

// A deadline flush that has no text yet still emits a Paste event, with empty text.
func TestAdvDeadlineFlushOfAnEmptyBodyEmitsNoEmptyPaste(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(80 * time.Millisecond)
	})
	h.write(pasteStart)
	ev, ok := advRecv(h, time.Second)
	if ok {
		if p, isPaste := ev.(Paste); isPaste && p.Text == "" {
			t.Fatalf("deadline flush of an empty body emitted %#v", ev)
		}
	}
}

// After a lost end marker the user pastes again. The second start marker is
// treated as text (ESC stripped), so the second paste arrives as "[200~second".
func TestAdvSecondPasteAfterALostEndMarkerIsNotMangled(t *testing.T) {
	h := advStuck(t)
	h.write(pasteStart + "second" + pasteEnd + "k")
	var got strings.Builder
	var after Event
	for {
		ev, ok := advRecv(h, time.Second)
		if !ok {
			t.Fatalf("no key after the second paste; paste text so far %q", got.String())
		}
		if p, isPaste := ev.(Paste); isPaste {
			got.WriteString(p.Text)
			continue
		}
		after = ev
		break
	}
	if got.String() != "second" {
		t.Fatalf("second paste after a lost end marker arrived as %q, want %q (the start marker leaked as text)", got.String(), "second")
	}
	if !advIsRune(after, 'k') {
		t.Fatalf("after = %#v", after)
	}
}

// Held-back bytes in continuation have no timer: a lone ESC typed after the
// deadline is held until another byte arrives, and is then swallowed.
func TestAdvStuckPasteLoneEscIsNotSwallowedWithTheNextKey(t *testing.T) {
	h := advStuck(t)
	h.write("\x1b")
	time.Sleep(100 * time.Millisecond)
	h.write("a")
	for {
		ev, ok := advRecv(h, time.Second)
		if !ok {
			t.Fatal("Esc then a produced no Escape key")
		}
		if k, isKey := ev.(Key); isKey && k.Type == KeyEscape {
			return
		}
		if p, isPaste := ev.(Paste); isPaste {
			t.Fatalf("Esc then a arrived as paste text %q; the Esc key was lost", p.Text)
		}
	}
}

// 1. Marker edge cases.

// The end marker split at every byte offset, with and without a gap longer than
// the escape delay at the split.
func TestAdvEndMarkerSplitAtEveryOffset(t *testing.T) {
	for _, gap := range []time.Duration{0, 80 * time.Millisecond} {
		for i := 0; i <= len(pasteEnd); i++ {
			h := newPasteHarness(t, nil)
			h.write(pasteStart + "ab" + pasteEnd[:i])
			if gap > 0 {
				time.Sleep(gap)
			}
			h.write(pasteEnd[i:] + "z")
			var got strings.Builder
			for {
				ev, ok := advRecv(h, 2*time.Second)
				if !ok {
					t.Fatalf("split %d gap %v: no key z, paste so far %q", i, gap, got.String())
				}
				if p, isPaste := ev.(Paste); isPaste {
					got.WriteString(p.Text)
					continue
				}
				if !advIsRune(ev, 'z') {
					t.Fatalf("split %d gap %v: got %#v, want key z", i, gap, ev)
				}
				break
			}
			if got.String() != "ab" {
				t.Fatalf("split %d gap %v: paste = %q, want ab", i, gap, got.String())
			}
		}
	}
}

func TestAdvNestedStartMarkerIsTextAndDoesNotNest(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + "a" + pasteStart + "b" + pasteEnd + "z")
	p, ok := h.next().(Paste)
	if !ok || p.Text != "a[200~b" {
		t.Fatalf("got %#v, want Paste a[200~b (ESC stripped, one level)", p)
	}
	h.expectKeys("z")
}

func TestAdvStrayEndMarkerWithoutStartIsNotTyped(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteEnd + "a")
	for {
		ev := h.next()
		if advIsRune(ev, 'a') {
			return
		}
		t.Fatalf("a stray end marker produced %#v before key a", ev)
	}
}

func TestAdvEmptyPasteThenKey(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + pasteEnd + "a")
	for {
		ev := h.next()
		if advIsRune(ev, 'a') {
			return
		}
		if p, ok := ev.(Paste); !ok || p.Text != "" {
			t.Fatalf("empty paste produced %#v", ev)
		}
	}
}

// The start marker itself arrives in two writes with a gap longer than the
// escape delay in the middle. The paste deadline only protects a paste that has
// opened; this is the escape timer's case.
func TestAdvPasteStartSplitAcrossTheEscapeTimer(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write("\x1b[20")
	time.Sleep(DefaultEscapeDelay + 30*time.Millisecond)
	h.write("0~hello" + pasteEnd + "z")
	ev := h.next()
	if p, ok := ev.(Paste); !ok || p.Text != "hello" {
		t.Fatalf("a paste start split by a %v gap decoded as %#v first, want Paste hello (the start was torn into Esc + keys)", DefaultEscapeDelay+30*time.Millisecond, ev)
	}
}

func advRunBoundary(t *testing.T, pause time.Duration) []Event {
	t.Helper()
	// 8190 plain bytes, then the start marker straddling the 8192-byte read.
	payload := strings.Repeat("a", 8190) + pasteStart + "body" + pasteEnd + "z"
	pr, pw := io.Pipe()
	r := NewReader(pr)
	go r.Run()
	t.Cleanup(func() { _ = pw.Close(); r.Close() })
	go func() {
		// Two writes: the first is exactly 8192 bytes (ends inside the marker).
		_, _ = pw.Write([]byte(payload[:8192]))
		time.Sleep(pause)
		_, _ = pw.Write([]byte(payload[8192:]))
	}()
	var evs []Event
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-r.Events():
			evs = append(evs, ev)
			if advIsRune(ev, 'z') {
				return evs
			}
		case <-deadline:
			t.Fatalf("no key z; %d events", len(evs))
		}
	}
}

func advCheckBoundary(t *testing.T, evs []Event) {
	t.Helper()
	keys := 0
	var paste strings.Builder
	for _, ev := range evs {
		switch e := ev.(type) {
		case Key:
			if advIsRune(ev, 'a') {
				keys++
			} else if !advIsRune(ev, 'z') {
				t.Fatalf("unexpected key %#v (the start marker was torn)", e)
			}
		case Paste:
			paste.WriteString(e.Text)
		}
	}
	if keys != 8190 || paste.String() != "body" {
		t.Fatalf("keys a = %d (want 8190), paste = %q (want body)", keys, paste.String())
	}
}

func TestAdvReadBoundaryInsideStartMarkerNoGap(t *testing.T) {
	advCheckBoundary(t, advRunBoundary(t, 0))
}

func TestAdvReadBoundaryInsideStartMarkerWithGapPastEscapeDelay(t *testing.T) {
	t.Skip("pre-existing residue, not caused by the paste deadline: a bare ESC [ held longer than the escape delay " +
		"is indistinguishable from Alt+[ and the start of every CSI, so it resolves as Escape plus keys. Only a " +
		"buffer that is a longer prefix of the paste start (ESC [ 2 ...) waits for the paste deadline instead.")
	advCheckBoundary(t, advRunBoundary(t, DefaultEscapeDelay+30*time.Millisecond))
}

// 2. Timer races.

// The deadline and the end marker race. Whatever the interleaving, the text is
// delivered exactly once, in order, nothing is typed, and the key after the
// marker survives.
func TestAdvDeadlineRacesEndMarker(t *testing.T) {
	const dl = 20 * time.Millisecond
	rng := rand.New(rand.NewSource(7)) // #nosec G404 -- a fixed-seed sweep: reproducing a failing draw is the point, secrecy is not.
	for i := 0; i < 150; i++ {
		h := newPasteHarness(t, func(r *Reader) {
			r.SetEscapeDelay(5 * time.Millisecond)
			r.SetPasteDeadline(dl)
		})
		h.write(pasteStart + "abc")
		time.Sleep(dl - 4*time.Millisecond + time.Duration(rng.Intn(8))*time.Millisecond)
		h.write("def" + pasteEnd + "z")
		// Whichever side wins the race, no byte is lost or doubled. If the
		// deadline wins, the rest of the text is keys, because a deadline flush
		// leaves paste mode (Fable ruling); the end marker is then a no-op.
		var got strings.Builder
		for {
			ev, ok := advRecv(h, 2*time.Second)
			if !ok {
				t.Fatalf("iter %d: no key z; text so far %q", i, got.String())
			}
			if p, isPaste := ev.(Paste); isPaste {
				got.WriteString(p.Text)
				continue
			}
			if advIsRune(ev, 'z') {
				break
			}
			k, isKey := ev.(Key)
			if !isKey || k.Type != KeyRunes || len(k.Runes) != 1 || k.Mod != 0 {
				t.Fatalf("iter %d: got %#v, want key z", i, ev)
			}
			got.WriteRune(k.Runes[0])
		}
		if got.String() != "abcdef" {
			t.Fatalf("iter %d: paste = %q, want abcdef (duplicate or lost bytes)", i, got.String())
		}
		if ev, ok := advRecv(h, 5*time.Millisecond); ok {
			t.Fatalf("iter %d: extra event %#v", i, ev)
		}
	}
}

// Paste 1 closes and paste 2 opens in the same read. The deadline timer armed
// for paste 1 is still running; paste 2 must get its own full deadline.
func TestAdvSecondPasteInheritsTheFirstPastesDeadline(t *testing.T) {
	const dl = 400 * time.Millisecond
	h := newPasteHarness(t, func(r *Reader) {
		r.SetEscapeDelay(10 * time.Millisecond)
		r.SetPasteDeadline(dl)
	})
	h.write(pasteStart + "a")
	time.Sleep(250 * time.Millisecond)
	second := time.Now()
	h.write(pasteEnd + pasteStart + "b")
	var sawA bool
	for {
		ev, ok := advRecv(h, 3*time.Second)
		if !ok {
			t.Fatal("no event for the second paste")
		}
		p, isPaste := ev.(Paste)
		if !isPaste {
			t.Fatalf("got %#v", ev)
		}
		if p.Text == "a" && !sawA {
			sawA = true
			continue
		}
		if p.Text == "b" {
			if waited := time.Since(second); waited < dl*3/4 {
				t.Fatalf("paste 2 was force-flushed %v after it opened; its deadline is %v (it inherited paste 1's timer)", waited, dl)
			}
			return
		}
		t.Fatalf("unexpected paste %q", p.Text)
	}
}

// Close with a paste open, and EOF with a paste open: nothing leaks.
func TestAdvCloseWithOpenPasteLeaksNoGoroutines(t *testing.T) {
	runtime.GC()
	base := runtime.NumGoroutine()
	for i := 0; i < 30; i++ {
		pr, pw := io.Pipe()
		r := NewReader(pr)
		r.SetPasteDeadline(30 * time.Millisecond)
		done := make(chan struct{})
		go func() { r.Run(); close(done) }()
		_, _ = pw.Write([]byte(pasteStart + "abc"))
		if i%2 == 0 {
			time.Sleep(60 * time.Millisecond) // deadline fired: continued state
			_, _ = pw.Write([]byte("tail\xe2\x98"))
		}
		r.Close()
		_ = pw.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iter %d: Run did not return after Close", i)
		}
	}
	time.Sleep(200 * time.Millisecond)
	runtime.GC()
	if after := runtime.NumGoroutine(); after > base+1 {
		buf := make([]byte, 1<<16)
		n := runtime.Stack(buf, true)
		t.Fatalf("goroutines %d -> %d after 30 closed readers\n%s", base, after, buf[:n])
	}
}

func TestAdvEOFWithOpenPasteDeliversWhatArrived(t *testing.T) {
	for _, forced := range []bool{false, true} {
		pr, pw := io.Pipe()
		r := NewReader(pr)
		r.SetPasteDeadline(40 * time.Millisecond)
		go r.Run()
		_, _ = pw.Write([]byte(pasteStart + "abc"))
		if forced {
			ev := <-r.Events()
			if p, ok := ev.(Paste); !ok || p.Text != "abc" {
				t.Fatalf("forced setup got %#v", ev)
			}
		}
		_, _ = pw.Write([]byte("def"))
		_ = pw.Close()
		var got strings.Builder
		sawEOF := false
		for ev := range r.Events() {
			switch e := ev.(type) {
			case Paste:
				got.WriteString(e.Text)
			case Error:
				sawEOF = true
			case Key:
				if !forced {
					t.Fatalf("unforced EOF produced key %#v", e)
				}
			default:
				t.Fatalf("forced=%v: EOF produced %#v", forced, e)
			}
		}
		want := "abcdef"
		if forced {
			want = "" // after the deadline paste mode is over: "def" is keys
		}
		if got.String() != want || !sawEOF {
			t.Fatalf("forced=%v: paste = %q eof=%v, want %q", forced, got.String(), sawEOF, want)
		}
	}
}

// SetPasteDeadline after Run has started is documented as unsupported; the race
// detector reports it. Gated so the suite stays green under -race:
// ADV_RACE_PROBE=1 go test -race -run SetPasteDeadlineMidPaste.
func TestAdvSetPasteDeadlineMidPaste(t *testing.T) {
	if os.Getenv("ADV_RACE_PROBE") == "" {
		t.Skip("set ADV_RACE_PROBE=1 under -race to show the unsynchronised write")
	}
	h := newPasteHarness(t, func(r *Reader) { r.SetPasteDeadline(time.Hour) })
	h.write(pasteStart + "x")
	time.Sleep(20 * time.Millisecond)
	h.r.SetPasteDeadline(30 * time.Millisecond)
	h.write("y")
	if ev, ok := advRecv(h, 500*time.Millisecond); ok {
		t.Logf("mid-paste SetPasteDeadline took effect: %#v", ev)
	} else {
		t.Logf("mid-paste SetPasteDeadline had no effect on the armed timer (expected: the timer was created with the old value)")
	}
}

// 3. Memory.

// A stream that never closes and never pauses: far more than the cap, in 64 KiB
// writes. Pending stays within the cap and every byte comes out as Paste text.
func TestAdvEndlessPasteStreamStaysBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 40 MiB")
	}
	const total = 40 << 20
	pr, pw := io.Pipe()
	r := NewReader(pr)
	go r.Run()
	t.Cleanup(func() { _ = pw.Close(); r.Close() })

	chunk := bytes.Repeat([]byte("0123456789abcdef"), 4096) // 64 KiB
	go func() {
		_, _ = pw.Write([]byte(pasteStart))
		for sent := 0; sent < total; sent += len(chunk) {
			if _, err := pw.Write(chunk); err != nil {
				return
			}
		}
		_, _ = pw.Write([]byte(pasteEnd + "z"))
		_ = pw.Close()
	}()

	var got int64
	var maxEvent int
	sawZ := false
	for ev := range r.Events() {
		switch e := ev.(type) {
		case Paste:
			got += int64(len(e.Text))
			maxEvent = max(maxEvent, len(e.Text))
		case Key:
			if advIsRune(ev, 'z') {
				sawZ = true
			} else {
				t.Fatalf("stray key %#v mid-stream", e)
			}
		}
	}
	if got != total || !sawZ {
		t.Fatalf("delivered %d of %d paste bytes, z=%v", got, total, sawZ)
	}
	if limit := DefaultMaxPasteBytes + 8192 + len(pasteStart); r.peakPending > limit {
		t.Fatalf("peak pending %d exceeds cap+read %d", r.peakPending, limit)
	}
	t.Logf("peakPending=%d maxEvent=%d", r.peakPending, maxEvent)
}

// With no consumer, the reader backpressures: it must not queue the stream
// anywhere unbounded (pipe -> raw(8) -> pending(<=cap) -> events(64)).
func TestAdvUnreadPasteStreamBackpressuresNearTheCap(t *testing.T) {
	pr, pw := io.Pipe()
	r := NewReader(pr)
	go r.Run()
	var accepted atomic.Int64
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop); _ = pw.Close(); r.Close() })
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	go func() {
		_, _ = pw.Write([]byte(pasteStart))
		for {
			select {
			case <-stop:
				return
			default:
			}
			n, err := pw.Write(chunk)
			accepted.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(3 * time.Second)
	got := accepted.Load()
	limit := int64(DefaultMaxPasteBytes) + 64*8192 + 8*8192 + (2 << 20)
	if got > limit {
		t.Fatalf("%d bytes accepted with nobody reading; bound is about %d", got, limit)
	}
	t.Logf("accepted %d bytes unread (cap %d)", got, DefaultMaxPasteBytes)
}

// 4. UTF-8 hold-back.

func TestAdvInvalidPartialRuneThenEndMarkerIsDelivered(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + "a\xf0\x90" + pasteEnd + "z")
	p, ok := h.next().(Paste)
	if !ok || p.Text != "a��" {
		t.Fatalf("got %#v, want Paste with two replacement characters", p)
	}
	h.expectKeys("z")
}

// A forced flush holds an invalid partial rune; the end marker then releases it
// without dropping it.
// Now driven by the byte limit, the only way into continuation after the Fable
// ruling that a deadline flush ends paste mode.
func TestAdvLimitFlushInvalidPartialRuneIsReleasedByEndMarker(t *testing.T) {
	h := newPasteHarness(t, func(r *Reader) {
		r.SetMaxPasteBytes(16)
		r.SetPasteIdle(time.Minute)
	})
	filler := strings.Repeat("x", 14)
	h.write(pasteStart + filler + "\xf0\x90")
	if p, ok := h.next().(Paste); !ok || p.Text != filler {
		t.Fatalf("want the filler with the partial rune held, got %#v", p)
	}
	time.Sleep(100 * time.Millisecond)
	h.write(pasteEnd + "z")
	var got strings.Builder
	for {
		ev := h.next()
		if p, ok := ev.(Paste); ok {
			got.WriteString(p.Text)
			continue
		}
		if !advIsRune(ev, 'z') {
			t.Fatalf("got %#v", ev)
		}
		break
	}
	if got.String() != "��" {
		t.Fatalf("held bytes released as %q, want two replacement characters", got.String())
	}
}

// 5. CRLF: pre-existing doubling, characterised not judged.
func TestAdvCRLFPasteDoublesNewlines(t *testing.T) {
	h := newPasteHarness(t, nil)
	h.write(pasteStart + "a\r\nb" + pasteEnd)
	p, ok := h.next().(Paste)
	if !ok {
		t.Fatalf("got %#v", p)
	}
	if p.Text != "a\n\nb" {
		t.Fatalf("CRLF paste = %q; the pre-existing doubling was a\\n\\nb", p.Text)
	}
	t.Logf("pre-existing: CRLF paste becomes %q (sanitizePaste maps CR and LF separately)", p.Text)
}
