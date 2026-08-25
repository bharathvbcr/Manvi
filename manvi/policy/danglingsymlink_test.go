package policy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"manvi/dc"
)

// Containment is a claim about where the kernel will put the bytes, not about
// how the path is spelled. A dangling symlink is the case where those two come
// apart most quietly: EvalSymlinks reports ENOENT for it exactly as it does for
// a name that was never created, so a normalizer that treats "did not resolve"
// as "does not exist yet" answers about the link's own name and never looks at
// where it points.
//
// The write that *creates* the target is the one that escapes. A second write,
// once the target exists, resolves normally and is refused — which is why this
// survived: every test that wrote twice saw a refusal.

func danglingTree(t *testing.T) (root, outside string) {
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

func TestADanglingSymlinkIsJudgedByWhereItPoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		link string
		// target is relative to the outside directory.
		target string
	}{
		{name: "link at the repository root", link: "notes.md", target: "authorized_keys"},
		{name: "link in a subdirectory", link: "src/notes.md", target: "authorized_keys"},
		{name: "link to a path under a missing outside dir", link: "notes.md", target: "deep/further/key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, outside := danglingTree(t)
			if dir := filepath.Dir(tc.link); dir != "." {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// The target does not exist: the link dangles.
			if err := os.Symlink(filepath.Join(outside, tc.target), filepath.Join(root, tc.link)); err != nil {
				t.Fatal(err)
			}

			normalized, outsideRoot := NormalizeRepoPath(root, tc.link)
			if !outsideRoot {
				t.Fatalf("NormalizeRepoPath(%q) reported the path contained as %q, but it points at %s",
					tc.link, normalized, filepath.Join(outside, tc.target))
			}

			// And the gate must refuse it, on the rung that means what it says.
			task := &dc.Task{
				ID:           "TASK-001",
				PlannedFiles: []dc.PlannedFile{{Path: tc.link, AllowedChange: dc.ChangeModify}},
			}
			d := FileGate{Root: root, HardRules: true}.EvaluateFileChange(tc.link, task, dc.OpWrite, false)
			if !d.Blocked() {
				t.Fatalf("the gate allowed a write through a dangling link out of the repository: %+v", d)
			}
			if d.Rule != RuleOutsideRoot {
				t.Errorf("refused as %q, want %q", d.Rule, RuleOutsideRoot)
			}
		})
	}
}

// A dangling link whose target is still inside the repository is an ordinary
// contained path, so the fix must not refuse it.
func TestADanglingSymlinkInsideTheRepoStaysContained(t *testing.T) {
	root, _ := danglingTree(t)
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "src", "generated.go"), filepath.Join(root, "link.go")); err != nil {
		t.Fatal(err)
	}
	normalized, outsideRoot := NormalizeRepoPath(root, "link.go")
	if outsideRoot {
		t.Fatalf("a dangling link to src/generated.go is contained; got outside (normalized %q)", normalized)
	}
	if normalized != "src/generated.go" {
		t.Errorf("normalized to %q, want the link's target src/generated.go", normalized)
	}
}

// A chain of links that never terminates must be refused rather than followed
// until something gives.
func TestASymlinkCycleIsRefusedRatherThanFollowed(t *testing.T) {
	root, _ := danglingTree(t)
	if err := os.Symlink(filepath.Join(root, "b"), filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "b")); err != nil {
		t.Fatal(err)
	}
	if _, outsideRoot := NormalizeRepoPath(root, "a"); !outsideRoot {
		t.Fatal("a symlink cycle must not be reported as a contained path")
	}
}

// Ordinary paths are unaffected: the common case is a file that simply does not
// exist yet, and a write gate is asked about those constantly.
func TestOrdinaryPathsStillNormalizeNormally(t *testing.T) {
	root, _ := danglingTree(t)
	for input, want := range map[string]string{
		"src/new.go":      "src/new.go",
		"./src/new.go":    "src/new.go",
		"a/b/c/deep.txt":  "a/b/c/deep.txt",
		"src/../other.go": "other.go",
	} {
		normalized, outsideRoot := NormalizeRepoPath(root, input)
		if outsideRoot {
			t.Errorf("NormalizeRepoPath(%q) reported outside; it is an ordinary contained path", input)
			continue
		}
		if normalized != want {
			t.Errorf("NormalizeRepoPath(%q) = %q, want %q", input, normalized, want)
		}
	}
	if _, outsideRoot := NormalizeRepoPath(root, "../escape.txt"); !outsideRoot {
		t.Error("a ..-traversal must still be reported outside")
	}
	_ = strings.TrimSpace
}
