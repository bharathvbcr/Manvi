package artifacts

import (
	"os"
	"path/filepath"
	"testing"
)

// A repository can commit its artifact directory as a symlink to anywhere. The
// store's own checks start below the root, so a root that is itself a link used
// to be walked as though it were the store: Create wrote wherever it pointed.
func TestAStoreRootThatIsASymlinkIsRefusedAndTheTargetStaysEmpty(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "artifacts")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	store, err := NewStore(link)
	if err == nil {
		_, err = store.Create("a/b.md", "planted", Metadata{})
	}
	if err == nil {
		t.Fatal("Create through a store root that is a symlink succeeded")
	}
	entries, readErr := os.ReadDir(outside)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("the symlink's target was written to: %v", entries)
	}
}

// The store's anchor is trusted and everything below it is not: a link at any
// component between them is refused, and nothing is created beyond it.
func TestAStoreUnderAnAnchorRefusesALinkAtAnyComponent(t *testing.T) {
	for _, tc := range []struct{ name, plant, rel string }{
		{"first component", ".devcouncil", ".devcouncil/artifacts"},
		{"middle component", "state/mid", "state/mid/artifacts"},
		{"last component", "artifacts", "artifacts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			anchor := filepath.Join(base, "repo")
			outside := filepath.Join(base, "outside")
			for _, d := range []string{filepath.Join(anchor, "state"), outside} {
				if err := os.MkdirAll(d, 0o750); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(anchor, filepath.FromSlash(tc.plant))); err != nil {
				t.Skipf("symlinks are not available: %v", err)
			}
			store, err := NewStoreUnder(anchor, tc.rel)
			if err == nil {
				if _, err = store.Create("a/b.md", "planted", Metadata{}); err == nil {
					t.Fatal("Create passed through a symbolic link")
				}
				if _, err := store.Get("a/b.md"); err == nil {
					t.Error("Get passed through a symbolic link")
				}
				if _, err := store.List(); err == nil {
					t.Error("List passed through a symbolic link")
				}
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 0 {
				t.Fatalf("the link's target was written to: %v", entries)
			}
		})
	}
}

// The check is made when the store is used, not when it is built, so a
// directory that is swapped for a link after the store exists is still refused.
func TestARootSwappedForALinkAfterTheStoreWasBuiltIsRefused(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "artifacts")
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o750); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create("first.md", "ok", Metadata{}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(base, "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, dir); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if _, err := store.Create("second.md", "planted", Metadata{}); err == nil {
		t.Error("Create followed a link swapped in after construction")
	}
	if _, err := store.Update("first.md", "planted", nil); err == nil {
		t.Error("Update followed a link swapped in after construction")
	}
	if _, err := store.Get("first.md"); err == nil {
		t.Error("Get followed a link swapped in after construction")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("the link's target was written to: %v", entries)
	}
}

// A relative store directory is checked from the working directory down.
func TestARelativeStoreDirRefusesALinkedParent(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	work := filepath.Join(base, "work")
	for _, d := range []string{outside, work} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(work, ".devcouncil")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	t.Chdir(work)
	store, err := NewStore("")
	if err == nil {
		_, err = store.Create("a.md", "planted", Metadata{})
	}
	if err == nil {
		t.Fatal("the default store directory passed through a linked .devcouncil")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("the link's target was written to: %v", entries)
	}
}

func TestNewStoreUnderRefusesAPathThatClimbsOut(t *testing.T) {
	if _, err := NewStoreUnder(t.TempDir(), "../x"); err == nil {
		t.Fatal("a store directory outside its anchor was accepted")
	}
}
