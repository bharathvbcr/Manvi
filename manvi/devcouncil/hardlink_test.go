package devcouncil

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/policy"
)

// A hard link is the aliasing case identity pinning cannot see. The pin asks
// "is this the file policy judged", and what policy judged is a name: `ln .env
// notes.txt` gives one inode two names, the ladder judges the innocent one,
// and the pin then guarantees the bytes reach the credential. These tests hold
// the rule that closes it — a write or a read whose target carries more than
// one name is an operation on a file the gate did not judge.

// linkSecret builds the laundering setup: a credential, and an innocent second
// name for it.
func linkSecret(t *testing.T, root, secret, alias, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, secret), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(root, secret), filepath.Join(root, alias)); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}
}

func TestPinnedWriteRefusesATargetWithASecondName(t *testing.T) {
	root := t.TempDir()
	linkSecret(t, root, ".env", "notes.txt", "API_KEY=supersecret\n")

	pinned, err := pinWriteTarget(root, "notes.txt")
	if err != nil {
		t.Fatalf("pinning the alias failed for the wrong reason: %v", err)
	}
	err = pinned.Write([]byte("innocent notes\n"), 0o644)
	if err == nil {
		t.Fatal("wrote through a hard link: the payload landed in .env under a name the gate never judged")
	}
	if !strings.Contains(err.Error(), "names reach this file") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	// The credential is untouched, which is the property that matters: the
	// refusal has to happen before the O_TRUNC, not after it.
	secret, readErr := os.ReadFile(filepath.Join(root, ".env"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(secret) != "API_KEY=supersecret\n" {
		t.Fatalf(".env was rewritten through its alias: %q", secret)
	}
}

func TestPinnedReadRefusesATargetWithASecondName(t *testing.T) {
	root := t.TempDir()
	linkSecret(t, root, ".env", "notes.txt", "API_KEY=supersecret\n")

	data, err := ReadPinned(root, "notes.txt", 0)
	if err == nil {
		t.Fatalf("read a credential through its alias: %q", data)
	}
	if strings.Contains(string(data), "supersecret") {
		t.Fatalf("the refusal still returned the secret: %q", data)
	}
	if !strings.Contains(err.Error(), "names reach this file") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}

// A single-named file must still be writable and readable: the rule is about
// aliasing, and a rung that refuses ordinary files is a rung operators turn off.
func TestPinnedWriteAndReadStillAllowASingleNamedFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "notes.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinned, err := pinWriteTarget(root, "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := pinned.Write([]byte("new\n"), 0o644); err != nil {
		t.Fatalf("an ordinary write was refused: %v", err)
	}
	data, err := ReadPinned(root, "notes.txt", 0)
	if err != nil || string(data) != "new\n" {
		t.Fatalf("an ordinary read was refused: %q %v", data, err)
	}
}

// TestGitStageRefusesAHardLinkedPath closes the other half of the same hole:
// the write gate is not the only way a credential reaches disk under a second
// name, and `git add notes.txt && git commit` puts it in history past both the
// secret patterns and .gitignore. Yolo posture, so nothing but this check can
// account for the refusal.
func TestGitStageRefusesAHardLinkedPath(t *testing.T) {
	f := newFixtureWith(t, map[string]string{flags.HarnessPosture: flags.PostureYolo})
	linkSecret(t, f.root, ".env", "notes.txt", "API_KEY=supersecret\n")
	checkout(t, f)

	res := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"notes.txt"}})
	if !res.IsError || res.Severity != string(policy.Hard) || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("staging a hard link to .env returned %s (rule=%q severity=%q)", res.Text, res.Rule, res.Severity)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("refusal is unparseable: %v (%q)", err, res.Text)
	}
	if paths, _ := payload["paths"].([]any); len(paths) != 1 || paths[0] != "notes.txt" {
		t.Errorf("refusal names %v, want the offending path", payload["paths"])
	}
	if staged := gitStagedNames(t, f.root); staged != "" {
		t.Errorf("the index was filled anyway: %q", staged)
	}
}

// TestGitCommitRefusesAHardLinkStagedElsewhere is the index-side twin: the
// staging did not go through devcouncil_git_stage, and the commit re-reads the
// index rather than trusting whatever filled it.
func TestGitCommitRefusesAHardLinkStagedElsewhere(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureStrict,
		flags.GrantsAgentCommands: "true",
	})
	linkSecret(t, f.root, ".env", "notes.txt", "API_KEY=supersecret\n")
	run(t, f.root, "add", "notes.txt")
	checkout(t, f)
	// Granted, so the command gate cannot be what refuses.
	grantCommand(t, f, renderCommandLine([]string{"git", "commit", "-m", "notes"}))

	res := f.call("devcouncil_git_commit", map[string]any{"message": "notes"})
	if !res.IsError || res.Rule != string(policy.RuleSecretPath) {
		t.Fatalf("committing a hard link to .env returned %s (rule=%q)", res.Text, res.Rule)
	}
	if n := headCount(t, f.root); n != 1 {
		t.Errorf("the credential reached history: %d commits exist", n)
	}
}

// TestSecretPathsFoldCase pins the second site of the case defect: the git
// tools and the write ladder consult one list, so they must read it the same
// way. This side did not fold, so ".ENV" reported clean here while the ladder
// refused it — and on APFS those are one file.
func TestSecretPathsFoldCase(t *testing.T) {
	for _, p := range []string{".ENV", ".Env", "server.KEY", "ID_RSA", "a.PEM", "Secrets/k", ".NPMRC"} {
		if leaked := secretPaths([]string{p}); len(leaked) != 1 {
			t.Errorf("secretPaths(%q) = %v, want it reported: the write ladder denies this path", p, leaked)
		}
	}
	// And the list still means what it says: an ordinary file is not a secret.
	if leaked := secretPaths([]string{"src/env.go", "notes.md"}); len(leaked) != 0 {
		t.Errorf("secretPaths over-reported: %v", leaked)
	}
}

func gitStagedNames(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "diff", "--cached", "--name-only").Output()
	if err != nil {
		t.Fatalf("diff --cached: %v", err)
	}
	return strings.TrimSpace(string(out))
}
