package devcouncil

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/childenv"
	"github.com/bharathvbcr/Manvi/manvi/childenv/childenvtest"
)

// A repository's alias, hook or filter runs with the git child's environment.
// An alias that prints it is that vector, run through the real seam — with and
// without the scratch index, which used to be the only case given an
// environment at all.
func TestGitChildrenDoNotSeeProviderCredentials(t *testing.T) {
	childenvtest.SetParent(t)
	for _, index := range []string{"", "/nonexistent/scratch-index"} {
		spec := gitSpec(t.TempDir(), index, []string{"-c", "alias.dumpenv=!env", "dumpenv"})
		spec.stdoutLimit = 1 << 20
		res := runProc(context.Background(), spec)
		if res.err != nil {
			t.Fatalf("index %q: %v (%s)", index, res.err, res.stderr)
		}
		dump := string(res.stdout)
		childenvtest.Check(t, dump)
		if index != "" && !strings.Contains(dump, "GIT_INDEX_FILE="+index) {
			t.Errorf("the scratch index did not reach git:\n%s", dump)
		}
	}
}

// os/exec reads a nil Env as "inherit everything", so a spec that names no
// environment is refused rather than run with this process's credentials.
func TestRunProcRefusesASpecWithNoEnvironment(t *testing.T) {
	res := runProc(context.Background(), procSpec{argv: []string{"true"}, timeout: 5 * time.Second})
	if res.err == nil || !strings.Contains(res.err.Error(), "environment") {
		t.Fatalf("runProc with a nil env = %v, want a refusal naming the environment", res.err)
	}
}

// The verifier is the operator's own tool and inherits the operator's
// environment, minus the provider credentials. A stand-in records what it saw.
func TestTheVerifierDoesNotSeeProviderCredentials(t *testing.T) {
	childenvtest.SetParent(t)
	dir := t.TempDir()
	dump := filepath.Join(dir, "env.txt")
	bin := filepath.Join(dir, "dcverify")
	script := "#!/bin/sh\nenv > '" + dump + "'\nprintf '{\"ok\":true,\"verifier\":\"dc-verify\",\"schema_version\":0}'\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a stand-in executable under t.TempDir must be executable
		t.Fatal(err)
	}
	_ = VerifierAvailable(context.Background(), bin)
	seen, err := os.ReadFile(dump) // #nosec G304 -- a file under this test's own t.TempDir
	if err != nil {
		t.Fatalf("the stand-in verifier did not run: %v", err)
	}
	childenvtest.Check(t, string(seen))
}

// The agent shell's allowlist is checked against the same catalogue the git
// and managed-agent environments are derived from.
func TestTheShellAllowlistNamesNoProviderCredential(t *testing.T) {
	for _, name := range childEnvAllowlist {
		if slices.Contains(childenv.Withheld(), name) {
			t.Errorf("%s is a provider credential and is on the shell allowlist", name)
		}
	}
}
