package artifacts

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Reads go through internal/treefile, which owns the two publish windows a
// reader can land in. A name replaced after the open is reopened rather than
// read as the old file (treefile TestANameReplacedAfterTheOpenIsNotReadAsTheOldFile).
// Where the OS has an atomic no-replace rename, a publish never gives the entry a
// second name (TestAPublishNeverLeavesTheEntryWithASecondLink). Where it does not
// (Windows always, Linux on EINVAL/ENOTSUP), Create still links from its stage,
// and a reader inside that window is refused rather than served; callers that
// must not fail retry the read. This package no longer accepts a temporary
// sibling as a second name. The tests that pinned that acceptance were retired
// in the 2026-11-24 reconcile of origin/main.

// The invariant the link-count check exists for: a name in the store that is
// also a name outside it is refused. No second name is accepted, so neither a
// plain link, nor a link beside a temporary-shaped decoy, nor a decoy that is a
// different file gets through.
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
