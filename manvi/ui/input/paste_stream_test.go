package input

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

// feedPaste delivers a bracketed paste of size bytes through a Reader in writes
// of chunk bytes, and returns the Paste text and how long it took to arrive.
func feedPaste(tb testing.TB, payload string, chunk int) (string, time.Duration) {
	tb.Helper()
	pr, pw := io.Pipe()
	r := NewReader(pr)
	go r.Run()
	defer r.Close()
	defer func() { _ = pw.Close() }()
	start := time.Now()
	go func() {
		for i := 0; i < len(payload); i += chunk {
			end := min(i+chunk, len(payload))
			if _, err := pw.Write([]byte(payload[i:end])); err != nil {
				return
			}
		}
	}()
	select {
	case ev := <-r.Events():
		p, ok := ev.(Paste)
		if !ok {
			tb.Fatalf("got %T, want a Paste", ev)
		}
		return p.Text, time.Since(start)
	case <-time.After(5 * time.Minute):
		tb.Fatal("no paste event")
		return "", 0
	}
}

func bracketed(body string) string { return pasteStart + body + pasteEnd }

// Each arriving chunk made decodePaste copy the whole pending buffer twice and
// search it again for the closing marker, so a paste cost the square of its size
// over the read size. This counts what the stream allocates: the quadratic path
// allocated tens of gigabytes for a 4 MB paste.
func TestPasteDecodeAllocatesLinearly(t *testing.T) {
	const size, chunk = 4 << 20, 1024
	payload := bracketed(strings.Repeat("x", size))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	text, _ := feedPaste(t, payload, chunk)
	runtime.ReadMemStats(&after)
	if len(text) != size {
		t.Fatalf("paste of %d bytes arrived as %d", size, len(text))
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if limit := uint64(16 * size); allocated > limit {
		t.Fatalf("a %d-byte paste in %d-byte chunks allocated %d MB (limit %d MB): decode is not linear in the paste",
			size, chunk, allocated>>20, limit>>20)
	}
}

// The closing marker is searched for once per byte, not once per chunk: the sum
// of what each search covered stays within a small multiple of the paste.
func TestPasteEndIsSearchedOncePerByte(t *testing.T) {
	const size, chunk = 1 << 20, 1024
	var searched int
	pasteSearchHook = func(n int) { searched += n }
	defer func() { pasteSearchHook = nil }()

	text, _ := feedPaste(t, bracketed(strings.Repeat("y", size)), chunk)
	if len(text) != size {
		t.Fatalf("paste of %d bytes arrived as %d", size, len(text))
	}
	if searched < size {
		t.Fatalf("searches covered %d bytes of a %d-byte paste: the closing marker was not looked for", searched, size)
	}
	// Each chunk re-covers the last len(pasteEnd)-1 bytes so a split marker is
	// found; nothing else is covered twice.
	if limit := size + (size/chunk+2)*len(pasteEnd)*2; searched > limit {
		t.Fatalf("searches covered %d bytes for a %d-byte paste (limit %d)", searched, size, limit)
	}
}

// The closing marker is found wherever the reads cut it, and bytes that only
// look like the start of it do not end the paste.
func TestPasteIsIdenticalWhereverTheReadsCutIt(t *testing.T) {
	bodies := []string{
		"plain",
		"line one\nline two\r\nline three\ttabbed",
		"looks like an end: \x1b[201 and \x1b[20 and \x1b[ and \x1b",
		strings.Repeat("0123456789", 37),
		"ends in a lone escape \x1b",
	}
	for _, body := range bodies {
		payload := bracketed(body)
		want, n := decode([]byte(payload), true)
		wantPaste, ok := want.(Paste)
		if !ok {
			t.Fatalf("one-shot decode of %q gave %T", body, want)
		}
		wantText := wantPaste.Text
		if n != len(payload) {
			t.Fatalf("one-shot decode of %q consumed %d of %d", body, n, len(payload))
		}
		for _, chunk := range []int{1, 2, 3, 5, 6, 7, 64} {
			if got, _ := feedPaste(t, payload, chunk); got != wantText {
				t.Fatalf("chunk=%d body=%q: got %q, want %q", chunk, body, got, wantText)
			}
		}
	}
}

// Two pastes in a row, the second cut mid-marker, each restart the search: the
// scan state of the first must not leak into the second.
func TestPasteScanDoesNotLeakIntoTheNextPaste(t *testing.T) {
	pr, pw := io.Pipe()
	r := NewReader(pr)
	go r.Run()
	defer r.Close()
	go func() {
		defer func() { _ = pw.Close() }()
		for _, s := range []string{
			pasteStart + "first, long enough to advance the scan" + pasteEnd[:3],
			pasteEnd[3:] + pasteStart + "second" + pasteEnd[:2],
			pasteEnd[2:] + "q",
		} {
			if _, err := pw.Write([]byte(s)); err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	var got []string
	deadline := time.After(10 * time.Second)
	for len(got) < 3 {
		select {
		case ev := <-r.Events():
			switch e := ev.(type) {
			case Paste:
				got = append(got, e.Text)
			case Key:
				got = append(got, string(e.Runes))
			}
		case <-deadline:
			t.Fatalf("events so far: %q", got)
		}
	}
	want := []string{"first, long enough to advance the scan", "second", "q"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// BenchmarkPasteDecode is the audit's 1 MB and 8 MB pastes in 1 KB chunks.
func BenchmarkPasteDecode(b *testing.B) {
	for _, mb := range []int{1, 8} {
		payload := bracketed(strings.Repeat("x", mb<<20))
		b.Run(fmt.Sprintf("%dMB_1KBchunks", mb), func(b *testing.B) {
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				text, _ := feedPaste(b, payload, 1024)
				if len(text) != mb<<20 {
					b.Fatalf("got %d bytes", len(text))
				}
			}
		})
	}
}
