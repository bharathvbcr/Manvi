//go:build unix

package input

import (
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

func TestReaderCloseAndWaitYieldsPipeWithoutStealingNextWrite(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	r := NewReader(pr)
	go r.Run()

	if _, err := pw.WriteString("a"); err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-r.Events():
		if !ok {
			t.Fatal("events closed before reading initial key")
		}
		k, ok := ev.(Key)
		if !ok || k.String() != "a" {
			t.Fatalf("got %#v, want Key 'a'", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial key")
	}

	// Stop the reader and wait for its read goroutine to exit poll(2).
	if !r.CloseAndWait() {
		t.Fatal("CloseAndWait returned false on a pollable pipe")
	}

	// The next write to the pipe must remain intact for a subsequent reader
	// (such as an external editor child process), never stolen by the closed Reader.
	const want = "vim-input-xyz"
	if _, err := pw.WriteString(want); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 64)
	readDone := make(chan string, 1)
	go func() {
		n, _ := pr.Read(buf)
		readDone <- string(buf[:n])
	}()

	select {
	case got := <-readDone:
		if got != want {
			t.Fatalf("subsequent reader got %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subsequent reader timed out: closed Reader stole or blocked pipe input")
	}
}

type fakePollNVAL struct {
	mu     sync.Mutex
	closed bool
}

func (*fakePollNVAL) waitReadable(<-chan struct{}) pollResult { return pollFallback }
func (f *fakePollNVAL) close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

type blockingSource struct {
	unblock chan struct{}
	once    sync.Once
}

func (b *blockingSource) Read([]byte) (int, error) {
	<-b.unblock
	return 0, io.EOF
}

func (b *blockingSource) release() {
	b.once.Do(func() { close(b.unblock) })
}

func TestReaderFallsBackOnPOLLNVALAndCloseAndWaitDoesNotHang(t *testing.T) {
	bs := &blockingSource{unblock: make(chan struct{})}
	t.Cleanup(bs.release)

	fp := &fakePollNVAL{}
	r := NewReader(bs)
	r.pollerFactory = func(io.Reader) (inputPoller, io.Closer, error) {
		return fp, nil, nil
	}
	go r.Run()

	// Give the read goroutine a moment to hit pollFallback and enter bs.Read.
	time.Sleep(20 * time.Millisecond)

	fp.mu.Lock()
	closed := fp.closed
	fp.mu.Unlock()
	if !closed {
		t.Fatal("pollFallback did not close poller before clearing it")
	}

	start := time.Now()
	if r.CloseAndWait() {
		t.Fatal("CloseAndWait returned true after pollFallback")
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		t.Fatalf("CloseAndWait blocked for %v after pollFallback", waited)
	}
}

type stuckPoller struct {
	gate chan struct{}
	once sync.Once
}

func (s *stuckPoller) waitReadable(<-chan struct{}) pollResult {
	<-s.gate
	return pollStopped
}

func (s *stuckPoller) close() {
	s.once.Do(func() { close(s.gate) })
}

func TestReaderCloseAndWaitTimesOutBounded(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()

	sp := &stuckPoller{gate: make(chan struct{})}
	t.Cleanup(sp.close)

	r := NewReader(pr)
	r.pollerFactory = func(io.Reader) (inputPoller, io.Closer, error) {
		return sp, nil, nil
	}
	go r.Run()

	// Ensure Run has registered cancellable=true before calling closeAndWait.
	time.Sleep(20 * time.Millisecond)

	start := time.Now()
	if r.closeAndWait(25 * time.Millisecond) {
		t.Fatal("closeAndWait returned true while poller was still stuck")
	}
	if waited := time.Since(start); waited > 250*time.Millisecond {
		t.Fatalf("closeAndWait took %v, exceeded bound", waited)
	}
}
