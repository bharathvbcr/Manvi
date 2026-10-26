package devcouncil

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// A bound the agent cannot cure must not refuse its commit. The bounds on
// collecting a secret file's values are not the agent's to cure — secret paths
// are write-protected, so it can neither shrink nor move the file — so each is
// a note on the result and on the session's degraded list instead. What the
// check still did is part of the note.

// requireNoted asserts the commit went through and says, on the result and in
// the degraded list the session log records, that a bound held back a check.
func requireNoted(t *testing.T, res tools.Result, kind, file string) {
	t.Helper()
	if res.IsError {
		t.Fatalf("a bound the agent cannot cure refused the commit: %s", res.Text)
	}
	id := "commit.scan." + kind + ": " + file
	if !slices.Contains(res.Degraded, id) {
		t.Errorf("Degraded = %q, want it to carry %q", res.Degraded, id)
	}
	for _, want := range []string{`"scan_notes"`, `"kind":"` + kind + `"`, `"file":"` + file + `"`, `"still_checked"`, `"operator_action"`} {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the result lacks %s: %s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "AUDIT") {
		t.Errorf("the result carries a value: %s", res.Text)
	}
}

func TestCommitGoesOnWhenASecretFileHasALineOverTheCap(t *testing.T) {
	t.Run("a binary whose first line is over the cap", func(t *testing.T) {
		f := scanFixture(t)
		trackSecret(t, f, "secrets/data.bin", strings.Repeat("A", 2<<20))
		write(t, f.root, "app.js", "const mode = 'development';\n")
		requireNoted(t, stageAndCommit(t, f, "app.js"), "line_cap", "secrets/data.bin")
	})
	t.Run("values before the line are still matched", func(t *testing.T) {
		f := scanFixture(t)
		trackSecret(t, f, "secrets/mixed.env", "API_TOKEN="+prodValue+"\n"+strings.Repeat("A", 2<<20))
		write(t, f.root, "app.js", "const mode = 'development';\n")
		requireNoted(t, stageAndCommit(t, f, "app.js"), "line_cap", "secrets/mixed.env")

		write(t, f.root, "notes.txt", "# notes\n"+prodBody+"EXTRA=1\n")
		requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "contains a value assigned in secrets/mixed.env")
	})
}

func TestCommitGoesOnWhenSecretFilesAssignMoreValuesThanTheCap(t *testing.T) {
	f := scanFixture(t)
	f.reg.scan.limits.secretValues = 1
	write(t, f.root, ".env", secretBody+"TOKEN=another-credential-9876543210\n")
	write(t, f.root, "app.js", "const mode = 'development';\n")
	requireNoted(t, stageAndCommit(t, f, "app.js"), "value_cap", ".env")

	write(t, f.root, "notes.txt", "# notes\n"+secretBody+"EXTRA=1\n")
	requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "contains a value assigned in .env")
}

func TestCommitGoesOnWhenSecretFilesExceedTheTotalCap(t *testing.T) {
	f := scanFixture(t)
	f.reg.scan.limits.secretTotalBytes = 50
	write(t, f.root, ".env", secretBody+"TOKEN=another-credential-9876543210\n")
	write(t, f.root, "app.js", "const mode = 'development';\n")
	requireNoted(t, stageAndCommit(t, f, "app.js"), "total_cap", ".env")

	write(t, f.root, "notes.txt", "# notes\n"+secretBody+"EXTRA=1\n")
	requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "contains a value assigned in .env")
}

// A tracked secret file the harness user cannot read used to fail the batch
// that hashes every secret file at once ("answered 1 ids for 2 files"), and with
// it every commit. Git holds its blobs, so it is judged by them.
func TestCommitGoesOnWhenATrackedSecretFileIsUnreadable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 file")
	}
	f := scanFixture(t)
	trackSecret(t, f, "secrets/locked.txt", prodBody)
	if err := os.Chmod(filepath.Join(f.root, "secrets", "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "app.js", "const mode = 'development';\n")
	requireNoted(t, stageAndCommit(t, f, "app.js"), "unreadable_worktree", "secrets/locked.txt")

	write(t, f.root, "notes.txt", "# notes\n"+prodBody+"EXTRA=1\n")
	requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "contains a value assigned in secrets/locked.txt")
}

// Every empty file has the same blob id, so a tracked empty secrets/.gitkeep made
// every new empty file "identical to secrets/.gitkeep". An empty file holds no
// credential to copy.
func TestCommitAllowsAnEmptyFileBesideATrackedEmptySecretPathFile(t *testing.T) {
	f := scanFixture(t)
	trackSecret(t, f, "secrets/.gitkeep", "")
	write(t, f.root, "placeholder.txt", "")
	if res := stageAndCommit(t, f, "placeholder.txt"); res.IsError {
		t.Fatalf("an empty file was refused as a copy of an empty secret-path file: %s", res.Text)
	}
}
