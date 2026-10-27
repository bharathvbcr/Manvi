package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every reason a read is refused says it is a read, names the file, and gives
// the person something to do about it. The link wording used to be the write
// path's ("refusing to write through ...").
func TestProjectInstructionsRefusalsSayReadAndNameARemedy(t *testing.T) {
	outside := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(outside, []byte("x = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		set  func(t *testing.T, root string)
		want []string
	}{
		"symlink out of the repository": {
			set: func(t *testing.T, root string) {
				if err := os.Symlink(outside, filepath.Join(root, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"refusing to read", "AGENTS.md", "path.outside_root", "Replace it with a copy of the file"},
		},
		"symlink inside the repository": {
			set: func(t *testing.T, root string) {
				writeFile(t, root, "real.md", "x = 1\n")
				if err := os.Symlink("real.md", filepath.Join(root, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{"refusing to read", "AGENTS.md", "symbolic link", "Replace the link with the file itself"},
		},
		"hard link": {
			set: func(t *testing.T, root string) {
				writeFile(t, root, "real.md", "x = 1\n")
				if err := os.Link(filepath.Join(root, "real.md"), filepath.Join(root, "AGENTS.md")); err != nil {
					t.Skipf("hard links are not available here: %v", err)
				}
			},
			want: []string{"refusing to read", "AGENTS.md", "path.aliased_read", "Replace it with a copy"},
		},
		"too large": {
			set: func(t *testing.T, root string) {
				writeFile(t, root, "AGENTS.md", strings.Repeat("x", maxProjectInstructionBytes+1))
			},
			want: []string{"refusing to read", "AGENTS.md", "exceeds"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := realRoot(t)
			c.set(t, root)
			chdir(t, root)
			_, faults := instructionsTurnAt(t, "anthropic", root)
			if !faultNaming(faults, c.want...) {
				t.Fatalf("no notice carries %q: %v", c.want, faults)
			}
			for _, f := range faults {
				if strings.Contains(f, "write") {
					t.Fatalf("a refused read speaks of writing: %q", f)
				}
			}
		})
	}
}
