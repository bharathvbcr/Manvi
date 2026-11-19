package artifacts

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// refused asserts err is a containment refusal, not some other failure that
// happens to stop the write: a refusal test that accepts any error also passes
// when the store is simply broken.
func refused(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, treefile.ErrRefused) {
		t.Errorf("%s: got %v, want a treefile refusal", what, err)
	}
}

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
	refused(t, "Create over a linked root", err)
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
			// The same layout without the link is a working store, so a refusal
			// below is the link's and not a broken store's.
			control := filepath.Join(base, "control")
			if err := os.MkdirAll(filepath.Join(control, "state"), 0o750); err != nil {
				t.Fatal(err)
			}
			good, err := NewStoreUnder(control, tc.rel)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := good.Create("a/b.md", "fine", Metadata{}); err != nil {
				t.Fatalf("the control store, with no link, cannot write: %v", err)
			}

			store, err := NewStoreUnder(anchor, tc.rel)
			if err != nil {
				refused(t, "NewStoreUnder", err)
			} else {
				_, err = store.Create("a/b.md", "planted", Metadata{})
				if err == nil {
					t.Fatal("Create passed through a symbolic link")
				}
				refused(t, "Create", err)
				_, err = store.Get("a/b.md")
				if err == nil {
					t.Error("Get passed through a symbolic link")
				}
				refused(t, "Get", err)
				_, err = store.List()
				if err == nil {
					t.Error("List passed through a symbolic link")
				}
				refused(t, "List", err)
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
	_, err = store.Create("second.md", "planted", Metadata{})
	if err == nil {
		t.Error("Create followed a link swapped in after construction")
	}
	refused(t, "Create", err)
	_, err = store.Update("first.md", "planted", nil)
	if err == nil {
		t.Error("Update followed a link swapped in after construction")
	}
	_, err = store.Get("first.md")
	if err == nil {
		t.Error("Get followed a link swapped in after construction")
	}
	refused(t, "Get", err)
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
	refused(t, "Create under a linked .devcouncil", err)
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

// A relative store directory (the default, ".devcouncil/artifacts", is one) is
// the common case, and must work end to end when nothing is linked: the
// refusals above mean something only beside this.
func TestARelativeStoreDirWorksEndToEnd(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, dir := range []string{"", "plain", filepath.Join("a", "b")} {
		store, err := NewStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Create("sub/x.md", "one", Metadata{Summary: "s"}); err != nil {
			t.Fatalf("NewStore(%q).Create: %v", dir, err)
		}
		if _, err := store.Update("sub/x.md", "two", nil); err != nil {
			t.Fatalf("NewStore(%q).Update: %v", dir, err)
		}
		got, err := store.Get("sub/x.md")
		if err != nil || got.Content != "two" || got.Revision != 2 {
			t.Fatalf("NewStore(%q).Get = %+v, %v", dir, got, err)
		}
		arts, err := store.List()
		if err != nil || len(arts) != 1 {
			t.Fatalf("NewStore(%q).List = %v, %v", dir, arts, err)
		}
	}
	under, err := NewStoreUnder(".", filepath.Join("rel", "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := under.Create("y.md", "x", Metadata{}); err != nil {
		t.Fatalf("NewStoreUnder with a relative anchor: %v", err)
	}
}
