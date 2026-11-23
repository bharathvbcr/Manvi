//go:build unix

package input

import (
	"os"
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
	r.CloseAndWait()

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
