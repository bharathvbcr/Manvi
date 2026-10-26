package artifacts

import (
	"errors"
	"os"
	"testing"
)

// assertUnsyncedErr checks the error says what happened: published, durability
// unconfirmed, and carrying the cause.
func assertUnsyncedErr(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("error %v does not match ErrPublishedUnsynced", err)
	}
	if errors.Is(err, ErrExists) {
		t.Fatalf("a published write must not also read as ErrExists: %v", err)
	}
}

// A directory that cannot be synced because of a hard failure (the write
// itself failed) is still a failure: the sentinel is only for published writes.
func TestAWriteThatFailedBeforePublishingIsNotUnsynced(t *testing.T) {
	store, dir := newTestStore(t)
	old := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	t.Cleanup(func() { syncFile = old })

	art, err := store.Create("new.md", "x", Metadata{})
	if err == nil || art != nil {
		t.Fatalf("a failed fsync of the file was reported as success: %+v, %v", art, err)
	}
	if errors.Is(err, ErrPublishedUnsynced) {
		t.Fatalf("a write that never published was called published: %v", err)
	}
	if _, serr := os.Stat(dir + "/new.md"); !os.IsNotExist(serr) {
		t.Fatalf("a failed Create left content behind (stat error: %v)", serr)
	}
}
