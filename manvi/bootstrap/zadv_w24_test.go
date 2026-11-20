package bootstrap

import (
	"os"
	"path/filepath"
	"testing"
)

// A link at every component of a nested store path is refused and nothing is
// made behind it.
func TestZadvW24EnsureRefusesALinkAtEveryComponentOfAStorePath(t *testing.T) {
	for _, linkAt := range []string{"a", "a/b", "a/b/c"} {
		t.Run(linkAt, func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			// make the components above the link for real
			parent := filepath.Dir(linkAt)
			if parent != "." {
				if err := os.MkdirAll(filepath.Join(root, parent), 0o755); err != nil { // #nosec G301 -- a directory under this test's own t.TempDir
					t.Fatal(err)
				}
			}
			if err := os.Symlink(outside, filepath.Join(root, linkAt)); err != nil {
				t.Skip(err)
			}
			report := Ensure(root, Options{
				StateDir:      ".devcouncil",
				ArtifactPaths: []string{filepath.Join(root, "a", "b", "c", "d", "store.sqlite")},
			})
			if entries, _ := os.ReadDir(outside); len(entries) != 0 {
				t.Fatalf("made %v behind a link at %s", entries, linkAt)
			}
			if len(report.Failures) == 0 {
				t.Fatalf("no failure reported for a link at %s: %+v", linkAt, report)
			}
		})
	}
}

// A dangling link component, and a link that stays inside the root, are refused too.
func TestZadvW24EnsureRefusesDanglingAndInternalLinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "absent"), filepath.Join(root, "dangling")); err != nil {
		t.Skip(err)
	}
	if err := os.Mkdir(filepath.Join(root, "real"), 0o755); err != nil { // #nosec G301 -- a directory under this test's own t.TempDir
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "internal")); err != nil {
		t.Fatal(err)
	}
	report := Ensure(root, Options{
		StateDir: ".devcouncil",
		ArtifactPaths: []string{
			filepath.Join(root, "dangling", "x", "s.sqlite"),
			filepath.Join(root, "internal", "x", "s.sqlite"),
		},
	})
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("made %v behind a dangling link", entries)
	}
	if len(report.Failures) != 2 {
		t.Errorf("want 2 failures (dangling, internal), got %d: %+v", len(report.Failures), report.Failures)
	}
}

// A RELATIVE state or store path that climbs out of the root is not an
// operator-named absolute directory: it is whatever a repository-supplied
// setting spelled, and must not be made by path.
func TestZadvW24EnsureDoesNotMakeARelativePathThatClimbsOutOfTheRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.Mkdir(root, 0o755); err != nil { // #nosec G301 -- a directory under this test's own t.TempDir
		t.Fatal(err)
	}
	for name, opts := range map[string]Options{
		"state dir":     {StateDir: "../escaped-state"},
		"artifact path": {StateDir: ".devcouncil", ArtifactPaths: []string{"../escaped-db/store.sqlite"}},
	} {
		report := Ensure(root, opts)
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			if e.Name() != "repo" {
				t.Errorf("%s: a directory %q was made outside the root (failures: %+v)", name, e.Name(), report.Failures)
				_ = os.RemoveAll(filepath.Join(base, e.Name()))
			}
		}
	}
}

// A relative StateDir below the root is made through the handle and reported
// by its relative spelling.
func TestZadvW24EnsureMakesARelativeStateDirBelowTheRoot(t *testing.T) {
	root := t.TempDir()
	report := Ensure(root, Options{StateDir: "var/state", ArtifactPaths: []string{"var/state/db/s.sqlite"}})
	if len(report.Failures) != 0 {
		t.Fatalf("failures: %+v", report.Failures)
	}
	if info, err := os.Stat(filepath.Join(root, "var", "state", "db")); err != nil || !info.IsDir() {
		t.Fatalf("not made: %v", err)
	}
	if len(report.CreatedDirs) != 2 || report.CreatedDirs[0] != filepath.Join("var", "state") {
		t.Errorf("CreatedDirs = %v", report.CreatedDirs)
	}
}
