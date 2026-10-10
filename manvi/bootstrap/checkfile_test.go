package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAStrayIgnoreCheckFileIsReported: trackedIgnored writes a temporary file
// into the operator's work tree. When it cannot take it away again the answer
// says so, rather than leaving a .gitignore-check-* behind without a word.
func TestAStrayIgnoreCheckFileIsReported(t *testing.T) {
	root := gitRepo(t)
	refused := errors.New("removal refused")
	var left string
	saved := removeCheckFile
	removeCheckFile = func(name string) error { left = name; return refused }
	t.Cleanup(func() {
		removeCheckFile = saved
		if left != "" {
			_ = os.Remove(left)
		}
	})

	_, err := trackedIgnored(t.Context(), root, "*.log\n")
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the failed removal reported", err)
	}
	if !strings.HasPrefix(filepath.Base(left), ".gitignore-check-") || !strings.Contains(err.Error(), left) {
		t.Fatalf("the report does not name the file left behind (%q): %v", left, err)
	}
}

// TestTheIgnoreCheckFileIsRemoved is the ordinary case the report above must
// not disturb: the answer comes back and nothing is left in the work tree.
func TestTheIgnoreCheckFileIsRemoved(t *testing.T) {
	root := gitRepo(t)
	if _, err := trackedIgnored(t.Context(), root, "*.log\n"); err != nil {
		t.Fatal(err)
	}
	left, err := filepath.Glob(filepath.Join(root, ".gitignore-check-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("check files left in the work tree: %v", left)
	}
}
