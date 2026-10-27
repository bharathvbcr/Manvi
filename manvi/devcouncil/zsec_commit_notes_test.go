package devcouncil

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
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

// `git ls-files --others` exits 0 with "warning: could not open directory
// 'secrets/': Permission denied" for an untracked directory the harness user
// cannot read, and leaves its files out of the listing. The listing then looks
// complete: a secret-path file there is never listed, hashed or compared. The
// warning is on stderr, which the listing used to drop, so the commit reported
// nothing at all. It is a note now, and the commit still goes through because
// the agent cannot cure a directory it cannot open.
func TestCommitNotesAnUntrackedDirectoryGitCouldNotOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0 directory")
	}
	lock := func(t *testing.T, f *fixture, rel string) {
		t.Helper()
		dir := filepath.Join(f.root, filepath.FromSlash(rel))
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: a directory, not a file; 0600 would make it unremovable
	}
	t.Run("a secret-path file inside it", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "secrets/prod.env", prodBody)
		write(t, f.root, "notes.txt", prodBody)
		lock(t, f, "secrets")
		res := stageAndCommit(t, f, "notes.txt")
		requireNoted(t, res, "unreadable_untracked_dir", "secrets/")
		for _, want := range []string{"Permission denied", "neither listed, hashed nor read"} {
			if !strings.Contains(res.Text, want) {
				t.Errorf("the note lacks %q: %s", want, res.Text)
			}
		}
	})
	t.Run("no secret-path file visible at all", func(t *testing.T) {
		f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
		checkout(t, f)
		write(t, f.root, "hidden/data.txt", "x\n")
		write(t, f.root, "app.js", "const mode = 'development';\n")
		lock(t, f, "hidden")
		requireNoted(t, stageAndCommit(t, f, "app.js"), "unreadable_untracked_dir", "hidden/")
	})
	t.Run("a nested directory is named once", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "a/b/c.txt", "x\n")
		write(t, f.root, "app.js", "const mode = 'development';\n")
		lock(t, f, "a/b")
		res := stageAndCommit(t, f, "app.js")
		requireNoted(t, res, "unreadable_untracked_dir", "a/b/")
		if n := strings.Count(res.Text, `"file":"a/b/"`); n != 1 {
			t.Errorf("a/b/ is named %d times, want once (two listings both warn): %s", n, res.Text)
		}
	})
	t.Run("a readable tree carries no such note", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "secrets/prod.env", prodBody)
		write(t, f.root, "app.js", "const mode = 'development';\n")
		res := stageAndCommit(t, f, "app.js")
		if res.IsError {
			t.Fatalf("an ordinary commit was refused: %s", res.Text)
		}
		for _, d := range res.Degraded {
			if strings.HasPrefix(d, "commit.scan.unreadable_untracked_dir") || strings.HasPrefix(d, "commit.scan.listing_warning") {
				t.Errorf("a readable tree degraded the scan: %q", res.Degraded)
			}
		}
		write(t, f.root, "notes.txt", prodBody)
		requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "secrets/prod.env")
	})
}

// A stderr line the scan does not recognise, a stderr cut by its cap and a
// flood of directories each stay a bounded, named note.
func TestListingWarningNotesAreBoundedAndNeverSilent(t *testing.T) {
	line := func(d string) string { return "warning: could not open directory '" + d + "': Permission denied\n" }
	t.Run("a clean listing has no note", func(t *testing.T) {
		if n := listingWarningNotes([]gitCapture{{out: "a\x00"}, {}}); len(n) != 0 {
			t.Fatalf("notes for a clean listing: %+v", n)
		}
	})
	t.Run("a directory in both listings is one note", func(t *testing.T) {
		n := listingWarningNotes([]gitCapture{{stderr: line("d/")}, {stderr: line("d/") + line("e/")}})
		if len(n) != 2 || n[0].File != "d/" || n[1].File != "e/" {
			t.Fatalf("notes = %+v", n)
		}
	})
	t.Run("an unrecognised warning is a listing note", func(t *testing.T) {
		n := listingWarningNotes([]gitCapture{{stderr: "avertissement: impossible d'ouvrir le répertoire 'd/'\n"}})
		if len(n) != 1 || n[0].Kind != "listing_warning" || !strings.Contains(n[0].Bound, "impossible") {
			t.Fatalf("notes = %+v", n)
		}
	})
	t.Run("a cut stderr is a listing note", func(t *testing.T) {
		n := listingWarningNotes([]gitCapture{{stderr: line("d/"), stderrNote: "stderr cut after 65536 bytes"}})
		if len(n) != 2 || n[1].Kind != "listing_warning" || !strings.Contains(n[1].Bound, "stderr cut after 65536 bytes") {
			t.Fatalf("notes = %+v", n)
		}
	})
	t.Run("a flood is capped and counted", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < maxListingNotes+5; i++ {
			b.WriteString(line("d" + strconv.Itoa(i) + "/"))
		}
		n := listingWarningNotes([]gitCapture{{stderr: b.String()}})
		if len(n) != maxListingNotes+1 || !strings.Contains(n[len(n)-1].File, "(5 more directories)") {
			t.Fatalf("%d notes, last %+v", len(n), n[len(n)-1])
		}
	})
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
