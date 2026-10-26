package devcouncil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// The commit content check reads a staged binary whole, in bounded windows, and
// reads a secret-path file from every version git holds. These tests run real
// git over real files; each case that was a lockout or a gap names it.

const (
	secretValue = "sk-live-AUDITsecretVALUE0123456789" //nolint:gosec // G101: a test fixture, not a credential
	// prodBody is a tracked secret file's whole content, with a value no other
	// fixture file shares, so the refusal can name which file it came from.
	prodValue = "sk-prod-AUDITproductionVALUE9876543210"
	prodBody  = "PROD_API_KEY=" + prodValue + "\n"
)

func zeros(n int) string { return strings.Repeat("\x00", n) }

// scanFixture is a dev-posture fixture holding a .env that assigns secretValue,
// with the task checked out and a KnownSecret that matches nothing — which
// still sends every binary through the window scan.
func scanFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	f.reg.deps.KnownSecret = func(string) bool { return false }
	checkout(t, f)
	return f
}

// trackSecret commits a file at a secret path directly, as a repository that
// already has one would. The tools refuse to stage it, which is the point.
func trackSecret(t *testing.T, f *fixture, name, body string) {
	t.Helper()
	write(t, f.root, name, body)
	run(t, f.root, "add", "-f", name)
	run(t, f.root, "commit", "-qm", "track "+name)
}

func requireCommitted(t *testing.T, f *fixture, path string) {
	t.Helper()
	if res := stageAndCommit(t, f, path); res.IsError {
		t.Fatalf("an ordinary commit was refused: %s", res.Text)
	}
}

func requireLeak(t *testing.T, res tools.Result, path, reason string) {
	t.Helper()
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) || res.Severity != string(policy.Hard) {
		t.Fatalf("a staged copy of the secret was committed: %+v", res)
	}
	if !strings.Contains(res.Text, path) || !strings.Contains(res.Text, reason) || strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Fatalf("the refusal should name %q and %q, never the value: %s", path, reason, res.Text)
	}
}

// Showing a binary as added lines made the patch the size of the binary, and
// the 16 MiB capture cap refused commits that only added, deleted or renamed
// one. The value scan reads what is added; a deletion adds nothing.
func TestCommitAllowsLargeBinaryAndTextChanges(t *testing.T) {
	t.Run("add a 20 MiB binary", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "big.bin", zeros(20<<20))
		requireCommitted(t, f, "big.bin")
	})
	t.Run("delete a 20 MiB binary", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "big.bin", zeros(20<<20))
		run(t, f.root, "add", "big.bin")
		run(t, f.root, "commit", "-qm", "add")
		run(t, f.root, "rm", "-q", "big.bin")
		if res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"}); res.IsError {
			t.Fatalf("deleting a large binary was refused: %s", res.Text)
		}
	})
	t.Run("rename a 9 MiB binary", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "old.bin", zeros(9<<20))
		run(t, f.root, "add", "old.bin")
		run(t, f.root, "commit", "-qm", "add")
		run(t, f.root, "mv", "old.bin", "new.bin")
		if res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"}); res.IsError {
			t.Fatalf("renaming a large binary was refused: %s", res.Text)
		}
	})
	t.Run("add forty 500 KiB binaries", func(t *testing.T) {
		f := scanFixture(t)
		var paths []string
		for i := 0; i < 40; i++ {
			name := fmt.Sprintf("b%02d.bin", i)
			write(t, f.root, name, zeros(500<<10))
			paths = append(paths, name)
		}
		if st := f.call("devcouncil_git_stage", map[string]any{"paths": paths}); st.IsError {
			t.Fatalf("staging was refused: %s", st.Text)
		}
		if res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"}); res.IsError {
			t.Fatalf("a commit of forty binaries was refused: %s", res.Text)
		}
	})
	t.Run("delete a 20 MiB text file", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "big.txt", strings.Repeat("a line of ordinary text, 0123456789\n", (20<<20)/37))
		run(t, f.root, "add", "big.txt")
		run(t, f.root, "commit", "-qm", "add")
		run(t, f.root, "rm", "-q", "big.txt")
		if res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"}); res.IsError {
			t.Fatalf("deleting a large text file was refused: %s", res.Text)
		}
	})
}

// A text patch the capture cap cut is read whole from the object store instead:
// no refusal, a note, and nothing past the cut goes unjudged.
func TestCommitScansWholeBlobsWhenThePatchOutgrowsItsCap(t *testing.T) {
	body := strings.Repeat("a line of ordinary text, 0123456789\n", (17<<20)/37)
	t.Run("clean", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "big.txt", body)
		res := stageAndCommit(t, f, "big.txt")
		if res.IsError {
			t.Fatalf("a clean large text file was refused: %s", res.Text)
		}
		if !strings.Contains(res.Text, "scan_notes") || !strings.Contains(res.Text, "scanned whole") {
			t.Errorf("the commit should say its text was scanned whole: %s", res.Text)
		}
	})
	t.Run("value after the cut", func(t *testing.T) {
		f := scanFixture(t)
		write(t, f.root, "big.txt", body+"key="+secretValue+"\n")
		requireLeak(t, stageAndCommit(t, f, "big.txt"), "big.txt", "contains a value assigned in .env")
	})
}

// A tracked vendor/cacert.pem is a secret path. At 300 KiB it was over the read
// cap, and every commit was refused with a remedy the write gate denies.
func TestCommitAllowsATrackedSecretPathFileOverTheOldReadCap(t *testing.T) {
	f := scanFixture(t)
	trackSecret(t, f, "vendor/cacert.pem", strings.Repeat("MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA0123456789\n", (300<<10)/54))
	write(t, f.root, "app.js", "const mode = 'development';\n")
	requireCommitted(t, f, "app.js")
}

// A tracked .env deleted from the working tree is judged by the blobs git holds,
// not refused: the agent cannot restore it (the write gate denies secret paths).
func TestCommitJudgesAMissingTrackedSecretByItsBlobs(t *testing.T) {
	f := scanFixture(t)
	trackSecret(t, f, ".env.production", prodBody)
	if err := os.Remove(filepath.Join(f.root, ".env.production")); err != nil {
		t.Fatal(err)
	}
	write(t, f.root, "app.js", "const mode = 'development';\n")
	requireCommitted(t, f, "app.js")

	write(t, f.root, "notes.txt", "# notes\n"+prodBody+"EXTRA=1\n")
	requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "contains a value assigned in .env.production")
}

// A tracked .env replaced in the working tree by an entry with no bytes to read
// (a symlink, a directory, a FIFO) is judged by its blobs. Its values used to
// be read from the working tree alone, so the replacement hid them.
func TestCommitJudgesAReplacedTrackedSecretByItsBlobs(t *testing.T) {
	replace := map[string]func(t *testing.T, path string){
		"symlink": func(t *testing.T, path string) {
			if err := os.Symlink("elsewhere.txt", path); err != nil {
				t.Skipf("no symlinks here: %v", err)
			}
		},
		"directory": func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o750); err != nil {
				t.Fatal(err)
			}
		},
		"fifo": func(t *testing.T, path string) {
			if err := runMkfifo(path, 0o600); err != nil {
				t.Skipf("no fifos here: %v", err)
			}
		},
	}
	for name, replaceWith := range replace {
		t.Run(name, func(t *testing.T) {
			f := scanFixture(t)
			trackSecret(t, f, ".env.production", prodBody)
			path := filepath.Join(f.root, ".env.production")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			replaceWith(t, path)
			write(t, f.root, "config.txt", "# notes\n"+prodBody+"EXTRA=1\n")
			requireLeak(t, stageAndCommit(t, f, "config.txt"), "config.txt", "contains a value assigned in .env.production")
		})
	}
}

// An unstaged edit that drops a key from a tracked .env leaves the value in HEAD
// and the index. A staged copy of it is still a copy.
func TestCommitJudgesTheCommittedVersionOfAnEditedSecret(t *testing.T) {
	f := scanFixture(t)
	trackSecret(t, f, ".env.production", prodBody)
	write(t, f.root, ".env.production", "NODE_ENV=development\n")
	write(t, f.root, "notes.txt", "# notes\n"+prodBody+"EXTRA=1\n")
	requireLeak(t, stageAndCommit(t, f, "notes.txt"), "notes.txt", "contains a value assigned in .env.production")
}
