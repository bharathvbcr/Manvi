//go:build unix

package treefile

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO at a name must be refused without blocking: opening one for reading
// waits for a writer that never comes.
func TestReadFileRefusesAFifoWithoutBlocking(t *testing.T) {
	anchor, _ := tree(t)
	if err := syscall.Mkfifo(filepath.Join(anchor, "pipe"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	done := make(chan error, 1)
	go func() {
		_, err := ReadFile(root, "pipe", ReadOptions{Limit: 10})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("got %v, want a refusal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked")
	}
}

// A character device the repository links to is a link, and is refused as one.
func TestReadFileRefusesALinkToADevice(t *testing.T) {
	anchor, _ := tree(t)
	link(t, os.DevNull, filepath.Join(anchor, "zero"))
	if err := os.Symlink("/dev/zero", filepath.Join(anchor, "z")); err != nil {
		t.Skip(err)
	}
	root, err := Open(anchor)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"zero", "z"} {
		if _, err := ReadFile(root, name, ReadOptions{Limit: 10}); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
