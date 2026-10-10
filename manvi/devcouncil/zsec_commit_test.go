package devcouncil

import (
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
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
