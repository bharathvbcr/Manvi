//go:build windows

package artifacts

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A lock file another writer has just deleted stays in the directory as a
// delete-pending entry until the last handle on it closes, and creating the name
// meanwhile fails with ERROR_ACCESS_DENIED, not "exists". lockEntry has to wait
// that out like any other contender; it used to return the error, which failed a
// writer that had only been unlucky in its timing.
func TestLockEntryWaitsOutADeletePendingLockFile(t *testing.T) {
	dir := t.TempDir()
	entry := filepath.Join(dir, "a.md")
	lockPath := filepath.Join(dir, "."+filepath.Base(entry)+"."+lockNonce+".tmp")

	name, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	// A handle that allows deleting, so os.Remove succeeds and leaves the name
	// delete-pending for as long as the handle stays open.
	h, err := syscall.CreateFile(name, syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.CREATE_NEW, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = syscall.CloseHandle(h)
		}
	}()
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	// The premise: creating the name now is refused with a code that is not
	// ErrExist. Without it this test would pass for the wrong reason.
	if f, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err == nil {
		_ = f.Close()
		t.Skip("this Windows does not leave a deleted file delete-pending while a handle is open")
	} else if !lockHeldElsewhere(err) {
		t.Fatalf("creating a delete-pending name failed with %v, which lockHeldElsewhere does not recognise", err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = syscall.CloseHandle(h)
	}()
	closed = true

	unlock, err := lockEntry(entry)
	if err != nil {
		t.Fatalf("lockEntry gave up on a lock file that was only delete-pending: %v", err)
	}
	unlock()
}
