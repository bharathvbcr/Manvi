package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// The directories Ensure makes below the root are made through handles, so a
// component that is a link out of the tree is refused and reported, not written
// through. MkdirAll by path followed it and made the directory outside.
func TestEnsureDoesNotMakeADirectoryThroughALinkOutOfTheTree(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "tmp")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	report := Ensure(root, Options{
		StateDir:      ".devcouncil",
		ArtifactPaths: []string{filepath.Join(root, "tmp", "store", "state.sqlite")},
	})
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatalf("a directory was made through the link: %v %v", entries, err)
	}
	if len(report.Failures) == 0 {
		t.Fatalf("the refusal was not reported: %+v", report)
	}
	if info, err := os.Stat(filepath.Join(root, ".devcouncil")); err != nil || !info.IsDir() {
		t.Fatalf("the state directory itself was not made: %v", err)
	}
}

// A state directory that is itself a link out of the tree was skipped as
// "already there" because Stat follows the link.
func TestEnsureReportsAStateDirectoryThatIsALink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".devcouncil")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	report := Ensure(root, Options{StateDir: ".devcouncil"})
	if len(report.Failures) == 0 {
		t.Fatalf("a linked state directory passed as present: %+v", report)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written through the link: %v", entries)
	}
}

// What was made is still reported, and an operator-named directory outside the
// root is still made.
func TestEnsureStillMakesAndReportsTheDirectories(t *testing.T) {
	root := t.TempDir()
	elsewhere := filepath.Join(t.TempDir(), "named", "by", "operator")
	report := Ensure(root, Options{
		StateDir:      ".devcouncil",
		ArtifactPaths: []string{filepath.Join(root, "var", "db", "state.sqlite"), filepath.Join(elsewhere, "g.json")},
	})
	if len(report.Failures) != 0 {
		t.Fatalf("failures: %+v", report.Failures)
	}
	for _, dir := range []string{filepath.Join(root, ".devcouncil"), filepath.Join(root, "var", "db"), elsewhere} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Errorf("%s was not made: %v", dir, err)
		}
	}
	if len(report.CreatedDirs) != 3 {
		t.Errorf("CreatedDirs = %v, want three", report.CreatedDirs)
	}
	again := Ensure(root, Options{
		StateDir:      ".devcouncil",
		ArtifactPaths: []string{filepath.Join(root, "var", "db", "state.sqlite"), filepath.Join(elsewhere, "g.json")},
	})
	if len(again.CreatedDirs) != 0 || len(again.Failures) != 0 {
		t.Errorf("a second run changed or failed: %+v", again)
	}
}

// A directory the operator named outside the repository is theirs to vouch for,
// including when it is itself a link (a state directory kept on another volume).
func TestEnsureTrustsAnOperatorNamedDirectoryThatIsALink(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	linked := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(real, linked); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	report := Ensure(root, Options{
		StateDir:      ".devcouncil",
		ArtifactPaths: []string{filepath.Join(linked, "graph", "g.json")},
	})
	if len(report.Failures) != 0 {
		t.Fatalf("an operator-named linked directory was refused: %+v", report.Failures)
	}
	if info, err := os.Stat(filepath.Join(real, "graph")); err != nil || !info.IsDir() {
		t.Fatalf("the directory was not made behind the operator's link: %v", err)
	}
}
