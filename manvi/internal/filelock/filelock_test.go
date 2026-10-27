//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly || illumos || windows

package filelock

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// A second holder waits until the first lets go, gives up when its context ends,
// and the lock is free again afterwards.
func TestLockIsExclusiveAndBoundedByItsContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	unlock, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := Lock(short, path); !errors.Is(err, context.DeadlineExceeded) {
		unlock()
		t.Fatalf("a second holder err = %v, want the context's deadline", err)
	}
	unlock()
	again, err := Lock(context.Background(), path)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	again()
}
