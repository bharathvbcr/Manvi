package bootstrap

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/internal/treefile"
)

// A .gitignore that is too large to be an ignore file is left alone and
// reported, not read whole into memory and rewritten.
func TestAnOversizedGitignoreIsLeftAloneAndReported(t *testing.T) {
	root := gitRepo(t)
	path := filepath.Join(root, ".gitignore")
	big := strings.Repeat("#\n", maxGitignoreBytes/2+1)
	if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}
	report := Ensure(root, options())
	if raw, err := os.ReadFile(path); err != nil || string(raw) != big { // #nosec G304 -- a path under this test's own t.TempDir
		t.Fatalf(".gitignore was changed (read error %v)", err)
	}
	if len(report.AddedRules) != 0 || len(report.Failures) == 0 {
		t.Fatalf("report = %+v, want a refusal and no added rules", report)
	}
	if !errors.Is(report.Failures[0].Err, treefile.ErrTooLarge) {
		t.Fatalf("failure = %v, want the size refusal", report.Failures[0].Err)
	}
}

// A link at .gitignore is a refusal, and the report says the file was left alone
// rather than that it could not be updated.
func TestALinkedGitignoreIsReportedAsLeftAlone(t *testing.T) {
	root := gitRepo(t)
	if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), filepath.Join(root, ".gitignore")); err != nil {
		t.Skipf("symlinks are not available: %v", err)
	}
	report := Ensure(root, options())
	if len(report.Failures) != 1 || !strings.Contains(report.Failures[0].What, "left alone") {
		t.Fatalf("failures = %v, want one saying .gitignore was left alone", report.Failures)
	}
	if !errors.Is(report.Failures[0].Err, treefile.ErrRefused) {
		t.Fatalf("failure = %v, want a refusal", report.Failures[0].Err)
	}
}

// The rules are written whether or not the directory sync is confirmed, and the
// sync is reported in its own words: a write that landed is not "could not be
// updated".
func TestAnUnconfirmedGitignoreSyncIsReportedAsPublished(t *testing.T) {
	prev := syncDir
	syncDir = func(*os.Root) error { return errors.New("disk gone") }
	t.Cleanup(func() { syncDir = prev })

	root := gitRepo(t)
	report := Ensure(root, options())
	if len(report.AddedRules) == 0 {
		t.Fatalf("the rules that were written were not reported as added: %+v", report)
	}
	if raw, err := os.ReadFile(filepath.Join(root, ".gitignore")); err != nil || !strings.Contains(string(raw), ".devcouncil") { // #nosec G304 -- a path under this test's own t.TempDir
		t.Fatalf(".gitignore lacks the rules (%v):\n%s", err, raw)
	}
	if len(report.Failures) != 1 || !errors.Is(report.Failures[0].Err, treefile.ErrPublishedUnsynced) ||
		!strings.Contains(report.Failures[0].What, "was updated") {
		t.Fatalf("failures = %v, want the unconfirmed sync", report.Failures)
	}
}
