package transport

import (
	"bufio"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

// BenchmarkSSEShortStream is one short streamed reply read to its end: the
// shape of most sub-agent turns, where the reader's buffer is far larger than
// anything the stream ever puts in it.
func BenchmarkSSEShortStream(b *testing.B) {
	var body strings.Builder
	for i := 0; i < 32; i++ {
		body.WriteString("data: {\"delta\":\"a few tokens of the answer\"}\n\n")
	}
	body.WriteString("data: [DONE]\n\n")
	payload := body.String()
	b.ReportAllocs()
	for b.Loop() {
		s := NewSSE(io.NopCloser(strings.NewReader(payload)), "[DONE]")
		for {
			if _, err := s.Next(); err != nil {
				if err != io.EOF {
					b.Fatal(err)
				}
				break
			}
		}
		_ = s.Close()
	}
}

func drainSSE(t *testing.T, s *SSE) ([]Event, error) {
	t.Helper()
	var events []Event
	for {
		ev, err := s.Next()
		if err != nil {
			return events, err
		}
		events = append(events, ev)
	}
}

// What Next returned must survive its buffer going to another stream. Every
// event is read out of the buffer, so an event that still pointed into it
// would be overwritten by whatever the next stream reads.
func TestEventsOutliveTheirRecycledBuffer(t *testing.T) {
	first := NewSSE(io.NopCloser(strings.NewReader(
		"event: a\ndata: first-stream-one\n\nevent: b\ndata: first-stream-two\ndata: continued\n\n")), "")
	events, err := drainSSE(t, first)
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if first.reader != nil {
		t.Fatal("a settled stream kept its buffer")
	}

	// Run several streams with different bytes through the pool, so whichever
	// buffer the first stream returned is overwritten.
	for i := 0; i < 4; i++ {
		other := NewSSE(io.NopCloser(strings.NewReader(
			"event: zzzzzzzzzz\ndata: XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX\n\n")), "")
		if _, err := drainSSE(t, other); err != io.EOF {
			t.Fatal(err)
		}
	}

	want := []Event{
		{Name: "a", Data: []byte("first-stream-one")},
		{Name: "b", Data: []byte("first-stream-two\ncontinued")},
	}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i := range want {
		if events[i].Name != want[i].Name || string(events[i].Data) != string(want[i].Data) {
			t.Errorf("event %d = %q/%q after its buffer was reused, want %q/%q",
				i, events[i].Name, events[i].Data, want[i].Name, want[i].Data)
		}
	}
}

// A settled stream goes on answering with its error, and hands its buffer back
// exactly once however many times it is asked: a buffer in the pool twice
// would be handed to two live streams at once.
func TestASettledStreamReleasesItsBufferOnce(t *testing.T) {
	for _, tc := range []struct {
		name, body, done string
		want             error
	}{
		{"sentinel", "data: x\n\ndata: [DONE]\n\n", "[DONE]", io.EOF},
		{"eof", "data: x\n\n", "", io.EOF},
		{"eof mid-event", "data: x\ndata: y", "", io.EOF},
		{"oversized frame", "data: " + strings.Repeat("x", MaxFrameBytes+1) + "\n\n", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewSSE(io.NopCloser(strings.NewReader(tc.body)), tc.done)
			_, err := drainSSE(t, s)
			if tc.want != nil && err != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if s.reader != nil {
				t.Fatal("a settled stream kept its buffer")
			}
			for i := 0; i < 3; i++ {
				if _, again := s.Next(); again != err {
					t.Fatalf("Next after settling = %v, want the settled %v", again, err)
				}
			}
			_ = s.Close()
			_ = s.Close()
			a := sseReaders.Get().(*bufio.Reader)
			b := sseReaders.Get().(*bufio.Reader)
			if a == b {
				t.Fatal("the pool handed out one buffer twice")
			}
			if a.Size() != sseReaderSize || b.Size() != sseReaderSize {
				t.Errorf("pooled buffers are %d and %d bytes, want %d", a.Size(), b.Size(), sseReaderSize)
			}
			sseReaders.Put(a)
			sseReaders.Put(b)
		})
	}
}

// Close is documented as safe to call from a second goroutine while a reader
// is blocked in Next. The buffer must stay with the reader through that, and
// the reader must see the same error it always did.
func TestCloseDuringABlockedNextLeavesTheBufferWithTheReader(t *testing.T) {
	pr, pw := io.Pipe()
	s := NewSSE(pr, "")
	go func() {
		_, _ = pw.Write([]byte("data: one\n\n"))
	}()
	if ev, err := s.Next(); err != nil || string(ev.Data) != "one" {
		t.Fatalf("first event = %q, %v", ev.Data, err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.Next()
		done <- err
	}()
	// The reader is now blocked inside the read; close it out from under it.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if err != io.ErrClosedPipe {
		t.Errorf("err = %v, want the closed pipe the body reports", err)
	}
	if s.reader != nil {
		t.Error("the reader settled but kept its buffer")
	}
	if _, again := s.Next(); again != err {
		t.Errorf("Next after settling = %v, want %v", again, err)
	}
}

// Many streams at once, each checking it read its own bytes: a buffer shared
// between two live streams shows up here as one stream's data in another.
func TestConcurrentStreamsReadTheirOwnBytes(t *testing.T) {
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for g := 0; g < 64; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for round := 0; round < 20; round++ {
				want := fmt.Sprintf("stream-%d-round-%d", g, round)
				var body strings.Builder
				for i := 0; i < 8; i++ {
					fmt.Fprintf(&body, "data: %s-%d\n\n", want, i)
				}
				s := NewSSE(io.NopCloser(strings.NewReader(body.String())), "")
				for i := 0; ; i++ {
					ev, err := s.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						errs <- err
						return
					}
					if got := string(ev.Data); got != fmt.Sprintf("%s-%d", want, i) {
						errs <- fmt.Errorf("read %q, want %s-%d", got, want, i)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
