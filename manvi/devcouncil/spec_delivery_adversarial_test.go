//go:build unix

package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/harness"
)

// Attack 6: a repository spec reaches every later turn's system prompt, so the
// bytes it is read from have to be the repository's own and not whatever a link
// points at. The spec reader is the skills reader; these pin that it stays so
// for the spec file shapes.
func TestARepositorySpecIsNeverReadThroughALinkOrPastItsBound(t *testing.T) {
	const doc = `{"name":"house","sections":[{"name":"style","order":60,"text":"OUTSIDE-MARKER"}]}`
	cases := map[string]func(t *testing.T, root, outside string){
		"symlinked file": func(t *testing.T, root, outside string) {
			advMust(t, os.MkdirAll(filepath.Join(root, harness.SpecsDir), 0o750))
			advMust(t, os.Symlink(filepath.Join(outside, "house.json"), filepath.Join(root, harness.SpecsDir, "house.json")))
		},
		"symlinked directory": func(t *testing.T, root, outside string) {
			advMust(t, os.MkdirAll(filepath.Join(root, ".devcouncil"), 0o750))
			advMust(t, os.Symlink(outside, filepath.Join(root, harness.SpecsDir)))
		},
		"hard link to a file outside": func(t *testing.T, root, outside string) {
			advMust(t, os.MkdirAll(filepath.Join(root, harness.SpecsDir), 0o750))
			advMust(t, os.Link(filepath.Join(outside, "house.json"), filepath.Join(root, harness.SpecsDir, "house.json")))
		},
		"symlink to a withheld file": func(t *testing.T, root, outside string) {
			advMust(t, os.WriteFile(filepath.Join(root, ".env"), []byte(doc), 0o600))
			advMust(t, os.MkdirAll(filepath.Join(root, harness.SpecsDir), 0o750))
			advMust(t, os.Symlink("../../.env", filepath.Join(root, harness.SpecsDir, "house.json")))
		},
		"over the bound": func(t *testing.T, root, outside string) {
			advMust(t, os.MkdirAll(filepath.Join(root, harness.SpecsDir), 0o750))
			big := `{"name":"house","lessons":"` + strings.Repeat("a", harness.MaxSpecBytes) + `"}`
			advMust(t, os.WriteFile(filepath.Join(root, harness.SpecsDir, "house.json"), []byte(big), 0o600))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSkillFixture(t)
			outside := t.TempDir()
			advMust(t, os.WriteFile(filepath.Join(outside, "house.json"), []byte(doc), 0o600))
			setup(t, f.root, outside)
			spec, found, err := harness.RepoSpecs{Read: f.reg.RepoReader(context.Background())}.Lookup("house")
			if found && err == nil {
				t.Fatalf("a spec was resolved from %s: %+v", name, spec)
			}
			if err == nil {
				t.Fatalf("%s was reported as 'no such spec' instead of refused", name)
			}
		})
	}
}
