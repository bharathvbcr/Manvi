package artifacts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A reader that opened an artifact just before Update renamed a new file over it
// holds an inode with no name left. That is the store's own write, not a
// planted link, and the read must come back with the artifact as published.
// Before openOrdinary reopened such a name, Get failed with "it has 0 hard
// links" under concurrent writers (TestCompareAndSwapAcrossStoresLosesNoUpdate,
// intermittent in CI).
func TestAReadThatLosesItsFileToAnUpdateReadsThePublishedOne(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows cannot rename over an open file, so a reader never holds an unlinked one (renameOverReaders waits instead)")
	}
	store, _ := newTestStore(t)
	if _, err := store.Create("a.md", "one", Metadata{}); err != nil {
		t.Fatal(err)
	}
	other, err := NewStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	readHook = func(rel string) {
		if rel != "a.md" || fired {
			return
		}
		fired = true
		if _, err := other.Update("a.md", "two", nil); err != nil {
			t.Errorf("update from the other store: %v", err)
		}
	}
	t.Cleanup(func() { readHook = nil })

	got, err := store.Get("a.md")
	if err != nil {
		t.Fatalf("a read that raced an update was refused: %v", err)
	}
	if !fired {
		t.Fatal("the hook never ran, so the window was not exercised")
	}
	if got.Content != "two" {
		t.Fatalf("content %q, want the published %q", got.Content, "two")
	}
}

// Create publishes by hard link and removes its temporary sibling a moment
// later; a reader in that moment sees two names, one of them the store's own
// temporary. That is not a link to a file elsewhere and must read.
func TestAReadDuringACreatesLinkWindowIsNotRefused(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("sub/a.md", "one", Metadata{}); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "sub", ".a.md.0123456789abcdef.tmp")
	if err := os.Link(filepath.Join(dir, "sub", "a.md"), sibling); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if got, err := store.Get("sub/a.md"); err != nil || got.Content != "one" {
		t.Fatalf("a file mid-publish was refused: %+v, %v", got, err)
	}
	if arts, err := store.List(); err != nil || len(arts) != 1 {
		t.Fatalf("List during the window: %v, %v", arts, err)
	}
}

// The invariant the link-count check exists for: a name in the store that is
// also a name outside it is refused. The accepted second name is the store's own
// temporary sibling and nothing else, so neither a plain link, nor a link
// beside a temporary-shaped decoy, nor a decoy that is a different file gets
// through.
func TestAHardLinkToAFileOutsideTheStoreIsStillRefused(t *testing.T) {
	store, dir := newTestStore(t)
	if _, err := store.Create("a.md", "one", Metadata{}); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	content := filepath.Join(dir, "a.md")
	if err := os.Remove(content); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, content); err != nil {
		t.Skipf("hard links unavailable across these directories: %v", err)
	}

	if got, err := store.Get("a.md"); err == nil {
		t.Fatalf("a hard link to a file outside the store was read: %+v", got)
	}

	// A temporary-shaped decoy that is a different file does not vouch for it.
	if err := os.WriteFile(filepath.Join(dir, ".a.md.0123456789abcdef.tmp"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get("a.md"); err == nil {
		t.Fatalf("a decoy sibling vouched for an outside link: %+v", got)
	}

	// A temporary-shaped name linked to the same outside file makes three names,
	// which is still not the two of a publish in flight.
	if err := os.Remove(filepath.Join(dir, ".a.md.0123456789abcdef.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, ".a.md.0123456789abcdef.tmp")); err != nil {
		t.Fatal(err)
	}
	if got, err := store.Get("a.md"); err == nil {
		t.Fatalf("a link plus a linked sibling was read: %+v", got)
	}
}

// A name is relative to the store on every platform: a leading separator or a
// drive is refused, and the name a caller gets back is slash-separated.
func TestNamesAreStoreRelativeAndSlashSeparatedOnEveryPlatform(t *testing.T) {
	store, _ := newTestStore(t)
	bad := []string{"/etc/passwd", `\etc\passwd`, `\\host\share\x.md`}
	if runtime.GOOS == "windows" {
		// Volumes exist only there; on Unix "C:x" is an ordinary file name.
		bad = append(bad, `C:foo.md`, `C:\x.md`)
	}
	for _, bad := range bad {
		if _, err := store.Create(bad, "x", Metadata{}); err == nil {
			t.Errorf("Create(%q) should be refused", bad)
		}
	}
	art, err := store.Create("lessons/a.md", "x", Metadata{})
	if err != nil || art.Name != "lessons/a.md" {
		t.Fatalf("Create: %+v, %v", art, err)
	}
}
