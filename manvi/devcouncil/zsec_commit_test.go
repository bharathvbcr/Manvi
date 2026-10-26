package devcouncil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// cp .env notes.txt, stage, commit: the staged name matches no secret pattern.
func TestCommitRefusesACopiedSecretUnderAnotherName(t *testing.T) {
	for _, posture := range []string{flags.PostureDev} {
		t.Run(posture, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: posture})
			write(t, f.root, ".env", secretBody)
			checkout(t, f)
			// What `cp .env notes.txt` leaves behind.
			write(t, f.root, "notes.txt", secretBody)

			stage := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"notes.txt"}})
			if stage.IsError {
				t.Fatalf("staging an ordinary-named file was refused: %s", stage.Text)
			}
			res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"})
			if !res.IsError || res.Rule != string(policy.RuleSecretPath) || res.Severity != string(policy.Hard) {
				t.Fatalf("the commit was not refused as a secret: %+v", res)
			}
			if !strings.Contains(res.Text, "notes.txt") || strings.Contains(res.Text, "AUDITsecretVALUE") {
				t.Fatalf("the refusal should name the path and never the value: %s", res.Text)
			}
			if out := gitStagedNames(t, f.root); !strings.Contains(out, "notes.txt") {
				t.Errorf("the refusal should leave the index as it found it, got %q", out)
			}
			head := f.call("devcouncil_git_log", map[string]any{"max": 5})
			if strings.Contains(head.Text, "innocent") {
				t.Fatalf("the commit landed: %s", head.Text)
			}
		})
	}
}

// An edited copy has a different blob id, but still holds the value.
func TestCommitRefusesAnEditedCopyOfASecret(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	checkout(t, f)
	write(t, f.root, "config.txt", "# notes\n"+secretBody+"EXTRA=1\n")
	f.call("devcouncil_git_stage", map[string]any{"paths": []string{"config.txt"}})
	res := f.call("devcouncil_git_commit", map[string]any{"message": "edited"})
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("an edited copy of .env was committed: %+v", res)
	}
}

// A known credential is refused in any file, even with no .env on disk.
func TestCommitRefusesAKnownCredentialValue(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	f.reg.deps.KnownSecret = func(text string) bool { return strings.Contains(text, "ghp_KNOWNtoken0123456789") }
	checkout(t, f)
	write(t, f.root, "script.sh", "curl -H 'Authorization: ghp_KNOWNtoken0123456789'\n")
	f.call("devcouncil_git_stage", map[string]any{"paths": []string{"script.sh"}})
	res := f.call("devcouncil_git_commit", map[string]any{"message": "script"})
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("a known credential was committed: %+v", res)
	}
}

// The check must not turn every commit into a refusal: an ordinary file, and
// a file that merely repeats configuration a .env also holds, still commit.
func TestCommitStillAcceptsOrdinaryContent(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", "NODE_ENV=development\n"+secretBody)
	checkout(t, f)
	write(t, f.root, "app.js", "const mode = 'development';\n")
	f.call("devcouncil_git_stage", map[string]any{"paths": []string{"app.js"}})
	res := f.call("devcouncil_git_commit", map[string]any{"message": "app"})
	if res.IsError {
		t.Fatalf("an ordinary commit was refused: %s", res.Text)
	}
}

// requireCommitRefusedUnchecked asserts the commit was refused because the
// content check could not run — not because it found a leak — and that the
// refusal says why, and that nothing landed.
func requireCommitRefusedUnchecked(t *testing.T, f *fixture, res tools.Result, reason ...string) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("the commit went through although the content check could not run: %s", res.Text)
	}
	for _, want := range append([]string{"the check did not run"}, reason...) {
		if !strings.Contains(res.Text, want) {
			t.Errorf("the refusal does not say %q: %s", want, res.Text)
		}
	}
	if strings.Contains(res.Text, "AUDITsecretVALUE") {
		t.Errorf("the refusal carries the value: %s", res.Text)
	}
	if head := f.call("devcouncil_git_log", map[string]any{"max": 5}); strings.Contains(head.Text, "innocent") {
		t.Fatalf("the commit landed: %s", head.Text)
	}
}

func stageAndCommit(t *testing.T, f *fixture, path string) tools.Result {
	t.Helper()
	if st := f.call("devcouncil_git_stage", map[string]any{"paths": []string{path}}); st.IsError {
		t.Fatalf("staging %q was refused: %s", path, st.Text)
	}
	return f.call("devcouncil_git_commit", map[string]any{"message": "innocent"})
}

// A secret-path file the check cannot stat is a file whose values it never
// read. That was a silent skip.
func TestCommitRefusesWhenASecretPathFileCannotBeStatted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads through a mode-0 directory")
	}
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, "secrets/token.txt", secretBody)
	run(t, f.root, "add", "secrets/token.txt")
	run(t, f.root, "commit", "-qm", "track the secret")
	checkout(t, f)
	write(t, f.root, "app.js", "const mode = 'development';\n")
	if st := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"app.js"}}); st.IsError {
		t.Fatalf("staging was refused: %s", st.Text)
	}
	dir := filepath.Join(f.root, "secrets")
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o750) })

	res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"})
	requireCommitRefusedUnchecked(t, f, res, "could not stat the secret-path file secrets/token.txt")
}

// A tracked secret-path file removed from the working tree is still listed by
// the index, and its blob id and values are what a staged copy under another
// name is judged against. Skipping it let `cp .env notes.txt; stage; rm .env`
// commit the copy. Restoring the file lets the check run, and then the copy is
// refused as a copy and an ordinary file commits.
func TestCommitRefusesAMissingTrackedSecretUntilItIsRestored(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	run(t, f.root, "add", "-f", ".env")
	run(t, f.root, "commit", "-qm", "track the secret")
	checkout(t, f)
	write(t, f.root, "notes.txt", secretBody)
	if err := os.Remove(filepath.Join(f.root, ".env")); err != nil {
		t.Fatal(err)
	}
	res := stageAndCommit(t, f, "notes.txt")
	requireCommitRefusedUnchecked(t, f, res, "the secret-path file .env is tracked but missing from the working tree", "git restore")

	run(t, f.root, "restore", ".env")
	if res := f.call("devcouncil_git_commit", map[string]any{"message": "innocent"}); !res.IsError ||
		res.Rule != string(policy.RuleSecretPath) || !strings.Contains(res.Text, "identical to .env") {
		t.Fatalf("with the file restored, the copy was not refused as a copy: %+v", res)
	}
	run(t, f.root, "reset", "-q", "notes.txt")
	write(t, f.root, "app.js", "const mode = 'development';\n")
	if res := stageAndCommit(t, f, "app.js"); res.IsError {
		t.Fatalf("an ordinary commit was refused with the secret restored: %s", res.Text)
	}
}

// A secret file over the read cap had its values skipped, so an edited copy of
// it sailed through. The refusal names the cap and what to do.
func TestCommitRefusesWhenASecretPathFileIsOverTheReadCap(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody+strings.Repeat("# padding\n", maxSecretFileBytes/len("# padding\n")+1))
	checkout(t, f)
	write(t, f.root, "config.txt", "# notes\n"+secretBody+"EXTRA=1\n")
	res := stageAndCommit(t, f, "config.txt")
	requireCommitRefusedUnchecked(t, f, res,
		"the secret-path file .env", fmt.Sprintf("over the %d-byte cap", maxSecretFileBytes), "its values were not scanned")
}

// git hashed the file a moment ago; this process then failed to read it.
func TestCommitRefusesWhenASecretPathFileCannotBeReadAfterHashing(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	checkout(t, f)
	f.reg.readSecretFile = func(string, int64) ([]byte, error) { return nil, errors.New("input/output error") }
	write(t, f.root, "app.js", "const mode = 'development';\n")
	res := stageAndCommit(t, f, "app.js")
	requireCommitRefusedUnchecked(t, f, res, "could not read the secret-path file .env", "input/output error")
}

// A binary file has no added lines in a plain patch, so a value inside one — or
// a copy of a secret file with a NUL appended, which has a new blob id — was
// never looked at.
func TestCommitRefusesABinaryCarryingASecretValue(t *testing.T) {
	const value = "sk-live-AUDITsecretVALUE0123456789"
	cases := []struct {
		name, file, body, attributes string
	}{
		{"edited copy with a trailing NUL", "copy.bin", secretBody + "\x00", ""},
		{"value inside binary data", "blob.bin", "\x00\x01\x02\xff" + value + "\x00\x03", ""},
		{"value on a line of binary data", "data.bin", "\x89PNG\r\n\x1a\n\x00\x00\nkey=" + value + "\n\x00", ""},
		{"file marked binary by gitattributes", "notes.dat", "# notes\n" + secretBody, "*.dat binary\n"},
		{"file marked -diff by gitattributes", "notes.dat", "# notes\n" + secretBody, "*.dat -diff\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
			write(t, f.root, ".env", secretBody)
			if c.attributes != "" {
				write(t, f.root, ".gitattributes", c.attributes)
			}
			checkout(t, f)
			write(t, f.root, c.file, c.body)
			res := stageAndCommit(t, f, c.file)
			if !res.IsError || res.Rule != string(policy.RuleSecretPath) || res.Severity != string(policy.Hard) {
				t.Fatalf("a binary carrying the secret was committed: %+v", res)
			}
			if !strings.Contains(res.Text, c.file) || !strings.Contains(res.Text, "contains a value assigned in .env") ||
				strings.Contains(res.Text, "AUDITsecretVALUE") {
				t.Fatalf("the refusal should name the path and the reason, never the value: %s", res.Text)
			}
		})
	}
}

// A staged patch that does not fit under the capture cap cannot be scanned in
// full, and a binary is part of the patch now. With values to look for, that is
// a refusal rather than a clean pass.
func TestCommitRefusesAStagedBinaryTooLargeToScan(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	checkout(t, f)
	write(t, f.root, "big.bin", strings.Repeat("\x00", maxGitCaptureBytes+1))
	res := stageAndCommit(t, f, "big.bin")
	requireCommitRefusedUnchecked(t, f, res, fmt.Sprintf("over the %d-byte cap", maxGitCaptureBytes), "was not scanned")
}

// A symlink at a secret path stores its target's name in git, not the secret,
// and has no bytes of its own to read; it is skipped, not refused.
func TestCommitIgnoresASymlinkAtASecretPath(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, "real.txt", secretBody)
	if err := os.Symlink("real.txt", filepath.Join(f.root, ".env")); err != nil {
		t.Skipf("no symlinks here: %v", err)
	}
	checkout(t, f)
	write(t, f.root, "app.js", "const mode = 'development';\n")
	if res := stageAndCommit(t, f, "app.js"); res.IsError {
		t.Fatalf("a symlink at a secret path refused an ordinary commit: %s", res.Text)
	}
}

// Showing binaries as text must not turn an ordinary binary into a refusal.
func TestCommitStillAcceptsACleanBinary(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
	write(t, f.root, ".env", secretBody)
	checkout(t, f)
	write(t, f.root, "logo.bin", "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x01\x02\n\xff\xfe")
	if res := stageAndCommit(t, f, "logo.bin"); res.IsError {
		t.Fatalf("a clean binary was refused: %s", res.Text)
	}
}

// The default posture is dev, which demotes soft rules. `cat .env` was only a
// soft command.no_lease, so it ran and printed the credential.
func TestShellReadsOfSecretPathsAreRefusedUnderEveryPostureButYolo(t *testing.T) {
	for _, posture := range []string{flags.PostureStrict, flags.PostureDev} {
		for _, tool := range []string{"cat", "head", "tail", "cp", "less", "base64"} {
			for _, target := range []string{".env", "secrets/token.txt"} {
				command := tool + " " + target
				if tool == "cp" {
					command += " copy.txt"
				}
				t.Run(posture+"/"+command, func(t *testing.T) {
					f := newFixtureWith(t, map[string]string{flags.HarnessPosture: posture})
					write(t, f.root, ".env", secretBody)
					write(t, f.root, "secrets/token.txt", secretBody)
					res := f.call("devcouncil_exec_command", map[string]string{"command": command})
					requireNoSecret(t, "exec_command", res)
					if !res.IsError || res.Rule != string(policy.RuleCommandSecretRead) || res.Severity != string(policy.Hard) {
						t.Fatalf("%q under %s: IsError=%v rule=%q severity=%q text=%s",
							command, posture, res.IsError, res.Rule, res.Severity, res.Text)
					}
				})
			}
		}
	}

	t.Run("yolo keeps its documented carve-out", func(t *testing.T) {
		f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
		write(t, f.root, ".env", secretBody)
		res := f.call("devcouncil_exec_command", map[string]string{"command": "cat .env"})
		if res.Rule == string(policy.RuleCommandSecretRead) {
			t.Fatalf("yolo applied a hard rule it turns off: %+v", res)
		}
	})
}

// The staged-content scan reads file names out of `git diff --cached`, whose
// spelling is repository configuration and git's own quoting. A copied secret
// is refused whatever the prefix setting and however git spells the name.
func TestCommitRefusesACopiedSecretWhateverTheDiffSpelling(t *testing.T) {
	cases := []struct {
		name   string
		config []string
	}{
		{"n.txt", []string{"diff.noprefix", "true"}},
		{"n.txt", []string{"diff.mnemonicPrefix", "true"}},
		{"café.txt", []string{"core.quotepath", "true"}},
		{"q\"uote.txt", nil},
		{"my notes.txt", nil},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%s %v", c.name, c.config), func(t *testing.T) {
			f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureDev})
			write(t, f.root, ".env", secretBody)
			checkout(t, f)
			if c.config != nil {
				run(t, f.root, "config", c.config[0], c.config[1])
			}
			write(t, f.root, c.name, "# edited\n"+secretBody+"X=1\n")
			if st := f.call("devcouncil_git_stage", map[string]any{"paths": []string{c.name}}); st.IsError {
				t.Fatalf("staging %q was refused: %s", c.name, st.Text)
			}
			res := f.call("devcouncil_git_commit", map[string]any{"message": "x"})
			if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
				t.Fatalf("a copied secret in %q was committed: %+v", c.name, res)
			}
			if !strings.Contains(res.Text, "contains a value assigned in .env") {
				t.Fatalf("the refusal does not say why: %s", res.Text)
			}
		})
	}
}

// An added line whose own text begins "++ " is spelled "+++ ..." in the patch
// and must not be taken for a file header that ends the file's content.
func TestScanAddedLinesTellsContentFromHeadersByHunkCounts(t *testing.T) {
	patch := "diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -0,0 +1,2 @@\n+++ not a header\n+SECRET\n"
	var got []string
	if err := scanAddedLines(patch, func(path, added string) { got = append(got, path+":"+added) }); err != nil {
		t.Fatal(err)
	}
	if want := []string{"x:++ not a header", "x:SECRET"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if err := scanAddedLines("@@ -0,0 +1 @@\n+orphan\n", func(string, string) {}); err == nil {
		t.Fatal("an added line with no readable file name was not an error")
	}
}
