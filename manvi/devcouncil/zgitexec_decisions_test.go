package devcouncil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
)

// These tests pin two owner/Fable decisions recorded in gitSpec's comment
// (2026-11-07): gated git leaves operator-configured commit signing and
// operator-configured clean filters alone. .git/config is a protected write
// path, so the program either names is operator-chosen, and the agent only
// routes content it already controls through it. They are characterisations:
// they pass on the base by design, and exist so that a later "hardening" that
// blanks gpg.program, adds --no-gpg-sign or neutralises filter.*.clean fails
// here instead of silently breaking signed agent commits or the operator's
// own clean filters.

func executableScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil { //nolint:gosec // the operator's program has to be executable
		t.Fatal(err)
	}
	return path
}

func TestGatedCommitKeepsTheOperatorsCommitSigning(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureYolo,
		flags.GrantsAgentCommands: "true",
	})
	outside := t.TempDir() // the fake signer and its marker stay out of the work tree
	marker := filepath.Join(outside, "signed.marker")
	// git reads the signature from stdout and the status line from the
	// --status-fd, which git passes as 2; it refuses a signer that omits
	// SIG_CREATED.
	signer := executableScript(t, outside, "fake-gpg",
		"cat >/dev/null\ntouch '"+marker+"'\n"+
			"echo '[GNUPG:] SIG_CREATED D 1 8 00 1 FFFF' >&2\n"+
			"printf -- '-----BEGIN PGP SIGNATURE-----\\n\\nZmFrZQ==\\n-----END PGP SIGNATURE-----\\n'\n")
	run(t, f.root, "config", "commit.gpgsign", "true")
	run(t, f.root, "config", "gpg.program", signer)
	run(t, f.root, "config", "user.signingkey", "ABCD")

	modifySeed(t, f)
	run(t, f.root, "add", "seed.txt")
	checkout(t, f)

	res := f.call("devcouncil_git_commit", map[string]string{"message": "signed by the operator's program"})
	if res.IsError {
		t.Fatalf("commit failed: %s", res.Text)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the operator's gpg.program did not run under the gated commit: %v", err)
	}
	if raw := gitOut(t, f.root, "cat-file", "-p", "HEAD"); !strings.Contains(raw, "gpgsig") {
		t.Errorf("the commit carries no signature header:\n%s", raw)
	}
}

func TestGatedStageRunsTheOperatorsCleanFilter(t *testing.T) {
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:      flags.PostureYolo,
		flags.GrantsAgentCommands: "true",
	})
	outside := t.TempDir()
	marker := filepath.Join(outside, "filter.marker")
	filter := executableScript(t, outside, "clean-filter", "touch '"+marker+"'\ncat\n")
	run(t, f.root, "config", "filter.opfilter.clean", filter)
	// The agent can write .gitattributes; the filter command is the operator's.
	if err := os.WriteFile(filepath.Join(f.root, ".gitattributes"), []byte("seed.txt filter=opfilter\n"), 0o644); err != nil { //nolint:gosec // an ordinary tracked file
		t.Fatal(err)
	}
	modifySeed(t, f)
	checkout(t, f)

	res := f.call("devcouncil_git_stage", map[string]any{"paths": []string{"seed.txt"}})
	if res.IsError {
		t.Fatalf("stage failed: %s", res.Text)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the operator's clean filter did not run under git_stage, which gitSpec documents it does: %v", err)
	}
}
