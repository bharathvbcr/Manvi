package devcouncil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The pin's contract is that a write lands where the policy layer said it
// would, whatever happens to the tree in between. The interesting case is the
// one where the target's parent directories do not exist yet, because then
// there is nothing to pin: resolvedDirs holds only the repository root, and
// the directories the write is about to create are chosen after the ladder has
// run and after any human has answered the approval prompt.
//
// writeFile pins before both of those, so "in between" is not microseconds —
// it is however long the dialog stays open.

// setupEscape builds a repo and an outside directory, and returns both.
func setupEscape(t *testing.T) (root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, outside
}

// A directory component planted as a symlink between pin and write must not be
// walked through. os.Mkdir answers EEXIST for a symlink, and treating that as
// "the directory is already there" is what let the write escape.
func TestPlantedSymlinkDirectoryCannotCarryAWriteOutOfTheRepo(t *testing.T) {
	for _, tc := range []struct {
		name string
		rel  string
		// plant is the component replaced by a symlink after pinning.
		plant string
	}{
		{name: "first component missing", rel: "newdir/file.txt", plant: "newdir"},
		{name: "deep tail missing", rel: "a/b/c/file.txt", plant: "a"},
		{name: "second component missing", rel: "sub/newdir/file.txt", plant: "sub/newdir"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := setupEscape(t)
			if strings.Contains(tc.plant, "/") {
				if err := os.MkdirAll(filepath.Join(root, filepath.Dir(tc.plant)), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			evil := filepath.Join(outside, "evil")
			if err := os.MkdirAll(evil, 0o755); err != nil {
				t.Fatal(err)
			}

			// Pin first — this is what writeFile does before the ladder runs.
			pinned, err := pinWriteTarget(root, tc.rel)
			if err != nil {
				t.Fatalf("pinning %q: %v", tc.rel, err)
			}

			// The attacker's move, during the approval window.
			if err := os.Symlink(evil, filepath.Join(root, tc.plant)); err != nil {
				t.Fatal(err)
			}

			writeErr := pinned.Write([]byte("ESCAPED"), 0o644)
			if writeErr == nil {
				t.Errorf("the write succeeded through a planted symlink component")
			}
			var refusal *symlinkRefusal
			if writeErr != nil && !errors.As(writeErr, &refusal) &&
				!strings.Contains(writeErr.Error(), "outside the repository") &&
				!strings.Contains(writeErr.Error(), "not a directory") {
				t.Logf("refused, though not as a symlink refusal: %v", writeErr)
			}

			// The filesystem is the arbiter: nothing may exist outside the repo.
			escaped := filepath.Join(evil, filepath.Base(tc.rel))
			if body, readErr := os.ReadFile(escaped); readErr == nil {
				t.Fatalf("a file escaped the repository to %s containing %q", escaped, body)
			}
			if err := filepath.Walk(outside, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				t.Errorf("a file was created outside the repository: %s", p)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The converse: creating missing directories is the ordinary case and must
// keep working, or the fix above is just a refusal.
func TestWriteStillCreatesMissingDirectories(t *testing.T) {
	root, _ := setupEscape(t)
	const rel = "a/b/c/file.txt"

	pinned, err := pinWriteTarget(root, rel)
	if err != nil {
		t.Fatalf("pinning: %v", err)
	}
	if err := pinned.Write([]byte("hello"), 0o644); err != nil {
		t.Fatalf("writing %q: %v", rel, err)
	}
	body, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("read %q, want %q", body, "hello")
	}
}

// A directory component that is a symlink to a directory *inside* the repo is
// refused too. It is contained, so it is not an escape — but it is a component
// whose identity the pin cannot hold, and the leaf rung already refuses a
// symlink for exactly that reason.
func TestSymlinkedDirectoryComponentIsRefusedEvenWhenContained(t *testing.T) {
	root, _ := setupEscape(t)
	if err := os.MkdirAll(filepath.Join(root, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	pinned, err := pinWriteTarget(root, "link/file.txt")
	if err != nil {
		t.Fatalf("pinning: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := pinned.Write([]byte("x"), 0o644); err == nil {
		t.Fatal("a symlinked directory component was walked through")
	}
	if _, err := os.Stat(filepath.Join(root, "real", "file.txt")); err == nil {
		t.Fatal("the write went through the link into real/")
	}
}
