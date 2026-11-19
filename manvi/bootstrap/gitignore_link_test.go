package bootstrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The .gitignore is the operator's file. One that is a link to somewhere else
// used to be read through and then replaced by a rename, which both copied the
// link target's text into the repository and dropped the link.
func TestAGitignoreThatIsALinkIsLeftAloneAndReported(t *testing.T) {
	root := gitRepo(t)
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, ".gitignore")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	report := Ensure(root, options())

	info, err := os.Lstat(link)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".gitignore is no longer a link: %v %v", info, err)
	}
	if len(report.AddedRules) != 0 {
		t.Errorf("rules were added to a link: %v", report.AddedRules)
	}
	if len(report.Failures) == 0 {
		t.Errorf("the refusal was not reported: %+v", report)
	}
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.Contains(p, string(filepath.Separator)+".git"+string(filepath.Separator)) {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		raw, rerr := os.ReadFile(p)
		if rerr == nil && strings.Contains(string(raw), "SECRET") {
			t.Errorf("%s holds the link target's text", p)
		}
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
}
