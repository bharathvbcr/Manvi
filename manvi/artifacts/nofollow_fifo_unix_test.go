//go:build !windows

package artifacts

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestGetDoesNotBlockOnAFIFO(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "a.md")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := store.Get("a.md")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Get returned a FIFO as an artifact")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Get blocked on a FIFO")
	}
}
