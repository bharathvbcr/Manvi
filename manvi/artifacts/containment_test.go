package artifacts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sanitizeName refuses traversal, and traversal is not the only way out of a
// directory. It rejects "../x" and an absolute path and then handed the result
// to os.WriteFile, which follows symbolic links — so a name carrying no ".."
// at all wrote wherever a planted link pointed.
//
// This store is reached by the create_artifact tool without passing the policy
// gate, so its own containment is the only thing between a model-chosen name
// and an arbitrary file write. That is why these are asserted against the
// filesystem rather than against the returned error: an error is what the code
// says happened, and the file is what happened.

func storeTree(t *testing.T) (store *Store, dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "artifacts")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir, outside
}

func TestNoArtifactWriteFollowsASymlinkOutOfTheStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		// plant is created as a symlink inside the store, pointing at target
		// under the outside directory.
		plant, target, artifact string
	}{
		{name: "leaf is a link", plant: "notes.md", target: "authorized_keys", artifact: "notes.md"},
		{name: "directory component is a link", plant: "sub", target: "d", artifact: "sub/x.md"},
		{name: "metadata sibling is a link", plant: "n.md.meta.json", target: "meta_target", artifact: "n.md"},
		{name: "deep directory component is a link", plant: "a", target: "d", artifact: "a/b/c.md"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir, outside := storeTree(t)
			target := filepath.Join(outside, tc.target)
			// A directory target for the directory-component cases, so the
			// escape would otherwise succeed rather than fail for an unrelated
			// reason.
			if strings.Contains(tc.artifact, "/") && !strings.HasSuffix(tc.plant, ".md") &&
				!strings.HasSuffix(tc.plant, ".json") {
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, tc.plant)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(dir, tc.plant)); err != nil {
				t.Fatal(err)
			}

			if _, err := s.Create(tc.artifact, "ESCAPED", Metadata{}); err == nil {
				t.Errorf("Create through a planted symlink succeeded")
			}
			if _, err := s.Update(tc.artifact, "ESCAPED", nil); err == nil {
				t.Errorf("Update through a planted symlink succeeded")
			}

			// The filesystem is the arbiter: nothing may appear outside.
			if err := filepath.Walk(outside, func(p string, info os.FileInfo, err error) error {
				if err != nil || info.IsDir() {
					return nil
				}
				body, _ := os.ReadFile(p)
				t.Errorf("a write escaped the store to %s containing %q", p, body)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The ordinary case must keep working, or the containment above is just a
// refusal. Nested names are legitimate and the store creates their directories.
func TestOrdinaryArtifactsStillRoundTrip(t *testing.T) {
	s, dir, _ := storeTree(t)

	if _, err := s.Create("plan.md", "first", Metadata{Summary: "a plan"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create("plan.md", "again", Metadata{}); err == nil {
		t.Error("creating an existing artifact should refuse")
	}
	updated, err := s.Update("plan.md", "second", nil)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated.Revision != 2 {
		t.Errorf("revision %d, want 2", updated.Revision)
	}
	got, err := s.Get("plan.md")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Content != "second" {
		t.Errorf("content %q, want %q", got.Content, "second")
	}

	if _, err := s.Create("design/api/v1.md", "nested", Metadata{}); err != nil {
		t.Fatalf("a nested name is legitimate: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "design", "api", "v1.md")); err != nil {
		t.Fatalf("the nested artifact was not written: %v", err)
	}

	// Traversal is still refused, by the rung that was already there.
	for _, bad := range []string{"../escape.md", "/etc/passwd", "a/../../escape.md", ""} {
		if _, err := s.Create(bad, "x", Metadata{}); err == nil {
			t.Errorf("Create(%q) should be refused", bad)
		}
	}
}
