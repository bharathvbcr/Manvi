package serve

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// work.verify.task hands its critic the diff of the change. A repository whose own
// config names a textconv driver for the changed files must not get that program
// run on the host to produce it, and the diff the critic reads must be the same
// text as before for an ordinary file.
func TestVerifyDiffForTheCriticNeverRunsTheRepositorysTextconvProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the textconv program is a shell script")
	}
	h := newVerifyHarness(t, []string{"adds A"}, nil)
	dir := t.TempDir()
	marker := filepath.Join(dir, "textconv-ran")
	script := filepath.Join(dir, "tc.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n: > '"+marker+"'\ncat \"$1\"\n"), 0o700); err != nil { // #nosec G306 -- a stand-in executable under t.TempDir must be executable
		t.Fatal(err)
	}
	writeFile(t, h.repo, ".gitattributes", "*.go diff=audit\n")
	gitIn(t, h.repo, "add", ".gitattributes")
	gitIn(t, h.repo, "commit", "-q", "-m", "attrs")
	gitIn(t, h.repo, "config", "diff.audit.textconv", script)
	h.change()
	h.settled("t", "req-textconv")

	if _, err := os.Stat(marker); err == nil {
		t.Fatal("work.verify.task ran the repository's diff.audit.textconv program to build the critic's diff")
	}
	h.critic.lastMu.Lock()
	prompt := h.critic.lastReq
	h.critic.lastMu.Unlock()
	if !strings.Contains(prompt, "+func A() int { return 1 }") {
		t.Fatalf("the critic's diff lost the change:\n%s", prompt)
	}
}
