package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolvePathStillDecidesAPinnedWrite is why write_file and patch_file
// call resolvePath even when the target is pinned and its result goes unused.
//
// The call looks dead on that branch: the pinned handle does the write, and
// `full` is read only when there is no pin. But resolvePath is a second
// containment judgement taken after the policy decision and any approval
// prompt, and it can fail there. If a directory on the path is swapped for a
// link out of the repository while the prompt is open, it is resolvePath that
// refuses, and its words are the refusal the model reads. Moving the call into
// the unpinned branch would leave the pinned write to refuse instead — still a
// refusal, but a different one, and one fewer independent check standing
// between a swapped path and a write.
func TestResolvePathStillDecidesAPinnedWrite(t *testing.T) {
	f := newFixture(t)
	outside := t.TempDir()
	writeRepoFile(t, f.root, "configs/app.conf", "old\n")

	pinned, err := pinWriteTarget(f.root, "configs/app.conf")
	if err != nil {
		t.Fatalf("pin: %v", err)
	}

	// The swap lands between the pin and the write, as it would while an
	// approval prompt sat open.
	if err := os.RemoveAll(filepath.Join(f.root, "configs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.root, "configs")); err != nil {
		t.Fatal(err)
	}

	_, resolveErr := f.reg.resolvePath("configs/app.conf")
	if resolveErr == nil {
		t.Fatal("resolvePath accepted a path that now leads out of the repository")
	}
	if !strings.Contains(resolveErr.Error(), "outside the repository") {
		t.Fatalf("resolvePath refused for an unexpected reason: %v", resolveErr)
	}

	writeErr := pinned.Write([]byte("new\n"), 0o644)
	if writeErr == nil {
		t.Fatal("the pinned write went through a swapped directory")
	}
	if writeErr.Error() == resolveErr.Error() {
		t.Fatalf("both checks refused in the same words (%v); this test no longer shows the call matters", writeErr)
	}
	if _, err := os.Stat(filepath.Join(outside, "app.conf")); !os.IsNotExist(err) {
		t.Fatalf("a file appeared outside the repository: %v", err)
	}
}
