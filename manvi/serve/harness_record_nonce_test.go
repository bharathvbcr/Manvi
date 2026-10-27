package serve

import (
	"os"
	"path/filepath"
	"testing"
)

// There is one record file per checkout directory. A checkout directory that is
// deleted and recreated is a new directory to the one-harness guard, which admits
// run B while run A is still live, and B's record replaces A's. When A ends, its
// restore must not take B's record away: B's block would be in the file with
// nothing to remove it.
func TestARunsEndDoesNotRemoveAnotherRunsRecord(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
	a, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	// The directory is deleted and recreated under run A: its block and record are gone.
	for _, rel := range []string{"CLAUDE.md", harnessPendingFile} {
		if err := os.Remove(filepath.Join(root, rel)); err != nil {
			t.Fatal(err)
		}
	}
	writeRaw(t, root, "CLAUDE.md", "rules\n")
	b, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil {
		t.Fatal(err)
	}
	live := readFile(t, root, "CLAUDE.md")

	if err := a.Restore(); err != nil {
		t.Fatalf("run A's restore: %v", err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != live {
		t.Fatalf("run A's restore touched run B's block:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("run A's end removed run B's record: %v", err)
	}
	if err := b.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "rules\n" {
		t.Fatalf("run B's block was not taken out: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !os.IsNotExist(err) {
		t.Fatal("run B's own end did not remove its record")
	}
}
