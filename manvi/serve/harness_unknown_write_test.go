package serve

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
)

// devcouncil.WritePinned reports an error after the new bytes are already in
// place when the directory sync fails (WriteMayHaveChanged). That is "unknown",
// not "failed": the one thing the materialisation may never do is delete the
// record of a block that may be in the file.

// failInstructionWrite makes the first write of rel report a failure after doing
// what do says to the file, and says whether the failure may have changed it.
func failInstructionWrite(t *testing.T, rel string, mayHaveChanged bool, do func(root string, data []byte)) {
	t.Helper()
	prev := pinnedWrite
	failed := false
	pinnedWrite = func(root, target string, data []byte) (bool, error) {
		if target != rel || failed {
			return prev(root, target, data)
		}
		failed = true
		do(root, data)
		return mayHaveChanged, errors.New("syncing the directory: injected")
	}
	t.Cleanup(func() { pinnedWrite = prev })
}

func landed(t *testing.T, rel string) func(root string, data []byte) {
	return func(root string, data []byte) {
		if err := devcouncil.WritePinned(root, rel, data, 0o644); err != nil {
			t.Error(err)
		}
	}
}

func TestAWriteThatLandedBeforeItsDirectorySyncFailedKeepsTheRecordAndTheRunGoesOn(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules"})
	failInstructionWrite(t, "CLAUDE.md", true, landed(t, "CLAUDE.md"))
	m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err != nil || m == nil {
		t.Fatalf("materialise = %v, %v: the block is in the file, so the run holds it", m, err)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("the record of a block that is in the file was removed: %v", err)
	}
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "rules" {
		t.Fatalf("the block was not taken out exactly: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the record outlived the restore")
	}
}

// A write that may have changed the file and did not leaves it as it was: that
// is a failed materialisation, with no record.
func TestAWriteThatMayHaveChangedTheFileButDidNotIsAFailure(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
	failInstructionWrite(t, "CLAUDE.md", true, func(string, []byte) {})
	m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err == nil || m != nil {
		t.Fatalf("materialise = %v, %v, want the failure", m, err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "rules\n" {
		t.Fatalf("the file changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a record of a block that is not there was left behind")
	}
}

// When the file cannot be read to tell, the answer is unknown, and the record
// stays. The run still gets what takes the block out, with the failure.
func TestAWriteWhoseOutcomeCannotBeReadKeepsTheRecord(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n", "README.md": "x\n"})
	failInstructionWrite(t, "CLAUDE.md", true, func(root string, data []byte) {
		landed(t, "CLAUDE.md")(root, data)
		// The file is now one the pinned reader refuses: a link.
		if err := os.Remove(filepath.Join(root, "CLAUDE.md")); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("README.md", filepath.Join(root, "CLAUDE.md")); err != nil {
			t.Error(err)
		}
	})
	m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules))
	if err == nil || m == nil {
		t.Fatalf("materialise = %v, %v, want the failure and a restore for the run to attempt", m, err)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("the record of a block that may be in the file was removed: %v", err)
	}
}

// A failure that says nothing changed is still a failure with no record.
func TestAWriteThatCannotHaveChangedTheFileLeavesNoRecord(t *testing.T) {
	root := scanRepo(t, map[string]string{"CLAUDE.md": "rules\n"})
	failInstructionWrite(t, "CLAUDE.md", false, func(string, []byte) {})
	if m, err := materialiseHarness(root, "claude", specWithSections(t, houseRules)); err == nil || m != nil {
		t.Fatalf("materialise = %v, %v, want the failure", m, err)
	}
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a record of a block that was never written was left behind")
	}
}

// Through a managed run: the block write lands but its outcome cannot be read.
// The run is aborted, finish cannot take the block out either, and the record,
// kept, is what the next attempt on the checkout takes it out with.
func TestManagedUnknownBlockWriteIsTakenOutByTheNextAttempt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permissions do not stop root from reading the file")
	}
	root := scanRepo(t, map[string]string{"CLAUDE.md": "project rules\n"})
	var atStart atomic.Value
	var starts atomic.Int32
	m := harnessLifecycle(t, root, "claude", &atStart, &starts)
	failInstructionWrite(t, "CLAUDE.md", true, func(root string, data []byte) {
		landed(t, "CLAUDE.md")(root, data)
		if err := os.Chmod(filepath.Join(root, "CLAUDE.md"), 0); err != nil {
			t.Error(err)
		}
	})
	if _, err := m.runner.Prepare(t.Context(), preparing("launch", `{"kind":"named","name":"registered"}`)); err == nil {
		t.Fatal("a run whose harness write could not be confirmed was prepared")
	}
	if starts.Load() != 0 {
		t.Fatal("a process was started for a run whose harness could not be confirmed")
	}
	m.finished()
	if _, err := os.Stat(filepath.Join(root, harnessPendingFile)); err != nil {
		t.Fatalf("the record of a block that may be in the file was removed: %v", err)
	}
	if e := m.runner.HarnessBusy(root); e == nil {
		t.Fatal("verification would diff a checkout that may carry the block")
	}
	if err := os.Chmod(filepath.Join(root, "CLAUDE.md"), 0o600); err != nil {
		t.Fatal(err)
	}
	next := &managedJob{done: make(chan struct{})}
	if err := m.runner.enterCheckout(next, managedRun{Cwd: root}); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, root, "CLAUDE.md"); got != "project rules\n" {
		t.Fatalf("the kept record did not take the block out: %q", got)
	}
}
