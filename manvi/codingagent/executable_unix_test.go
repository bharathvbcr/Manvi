//go:build !windows

package codingagent

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRequireNativeExecutableDoesNotBlockOnAFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "claude")
	if err := syscall.Mkfifo(fifo, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- requireNativeExecutable(fifo) }()
	select {
	case err := <-done:
		var refusal *NotNativeExecutableError
		if !errors.As(err, &refusal) {
			t.Fatalf("a FIFO was not refused: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the check blocked on a FIFO")
	}
}
