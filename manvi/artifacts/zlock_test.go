package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// An entry lock is held by the kernel, not by its name's age. A lock file that
// is 31 seconds old belongs to a live writer if that writer still holds it, so a
// contender must wait for it, and fail rather than take it over.
func TestALockFileOlderThanThirtySecondsIsNotTakenFromItsHolder(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "x", Metadata{}); err != nil {
		t.Fatal(err)
	}
	cPath := filepath.Join(dir, "a.md")
	unlock, err := store.lockEntry(cPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	lock := filepath.Join(dir, "."+"a.md"+"."+lockNonce+".tmp")
	old := time.Now().Add(-31 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}

	lockWait = 200 * time.Millisecond
	t.Cleanup(func() { lockWait = 10 * time.Second })
	second, err := store.lockEntry(cPath)
	if err == nil {
		second()
		t.Fatal("a second writer took a lock whose holder was still running")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("got %v, want ErrLocked", err)
	}
}
