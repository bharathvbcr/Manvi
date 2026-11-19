package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/artifacts"
)

func stateTree(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

func TestCheckStateDirBelowRefusesAnyWayOutOfTheRoot(t *testing.T) {
	root, outside := stateTree(t)
	if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "absent"), filepath.Join(root, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "real", "deep")); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".devcouncil", ".devcouncil/artifacts", "dangling", "real/deep/x", "../outside", "a/../../outside"} {
		if err := checkStateDirBelow(root, rel); !errors.Is(err, errStateDirOutside) {
			t.Errorf("checkStateDirBelow(%q) = %v, want errStateDirOutside", rel, err)
		}
	}
}

func TestCheckStateDirBelowAllowsWhatStaysInside(t *testing.T) {
	root, _ := stateTree(t)
	if err := os.Mkdir(filepath.Join(root, "real"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	// A project root that is itself reached through a link is resolved before
	// it is compared with what lies below it.
	viaLink := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, viaLink); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ root, rel string }{
		{root, ".devcouncil"},
		{root, "real"},
		{root, "alias/state"},
		{viaLink, ".devcouncil/artifacts"},
		{viaLink, "alias"},
	} {
		if err := checkStateDirBelow(c.root, c.rel); err != nil {
			t.Errorf("checkStateDirBelow(%q, %q) = %v, want nil", c.root, c.rel, err)
		}
	}
}

// An absolute MANVI_STATE_DIR is the operator's own statement and is used as
// given; the same destination reached through a link the repository committed
// is not.
func TestVerifyStateDirIsAboutRelativeValuesOnly(t *testing.T) {
	root, outside := stateTree(t)
	defer setProjectRootForTest(root)()
	if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}

	t.Setenv("MANVI_STATE_DIR", "")
	if err := verifyStateDir(); !errors.Is(err, errStateDirOutside) {
		t.Errorf("default state dir behind a committed link: %v", err)
	}
	t.Setenv("MANVI_STATE_DIR", ".devcouncil")
	if err := verifyStateDir(); !errors.Is(err, errStateDirOutside) {
		t.Errorf("relative state dir behind a committed link: %v", err)
	}
	t.Setenv("MANVI_STATE_DIR", outside)
	if err := verifyStateDir(); err != nil {
		t.Errorf("absolute state dir was examined: %v", err)
	}
}

func TestNewArtifactStoreRefusesALinkedStateDirAndWritesNothingThroughIt(t *testing.T) {
	root, outside := stateTree(t)
	defer setProjectRootForTest(root)()
	t.Setenv("MANVI_STATE_DIR", "")
	if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	store, err := newArtifactStore()
	if err == nil {
		_, err = store.Create("a/b.md", "x", artifacts.Metadata{})
	}
	if err == nil {
		t.Fatal("an artifact was created behind a committed .devcouncil link")
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("the link's target was written to: %v", entries)
	}
}
