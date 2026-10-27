//go:build unix

package codingagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFakeClaude(t *testing.T, body string) string {
	t.Helper()
	prog := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\n"+body), 0o700); err != nil { // #nosec G306 -- a script the test execs
		t.Fatal(err)
	}
	return prog
}

// A `claude --version` that exits 0 and prints its identity is a good probe
// even when a background process it started still holds stdout. Bounding the
// wait with WaitDelay made os/exec report ErrWaitDelay for that, and the probe
// treated every error as fatal, so a valid executable that base accepted after
// a delay was refused. The identity checks decide, not the pipe.
func TestClaudeBuildAcceptsAValidVersionWhoseDescendantHoldsStdoutPastTheWaitDelay(t *testing.T) {
	prog := writeFakeClaude(t, "echo '2.1.0 (Claude Code)'\n(sleep 4) &\nexit 0\n")
	started := time.Now()
	version, err := claudeBuild(context.Background(), prog)
	elapsed := time.Since(started)
	t.Logf("claudeBuild returned %q after %s: %v", version, elapsed.Round(10*time.Millisecond), err)
	if err != nil || version != "2.1.0" {
		t.Fatalf("version=%q err=%v, want 2.1.0 accepted", version, err)
	}
	if limit := claudeProbeWaitDelay + 1500*time.Millisecond; elapsed > limit {
		t.Fatalf("accepted after %s, want about the %s WaitDelay (limit %s)", elapsed, claudeProbeWaitDelay, limit)
	}
}

// What stays refused: a non-zero exit (even with the identity printed and a
// descendant on the pipe), and output that is not Claude Code's.
func TestClaudeBuildStillRefusesWhatIsNotAGoodProbe(t *testing.T) {
	for name, body := range map[string]string{
		"a non-zero exit":      "echo '2.1.0 (Claude Code)'\n(sleep 4) &\nexit 1\n",
		"a non-zero exit only": "echo '2.1.0 (Claude Code)'\nexit 1\n",
		"another program":      "echo '9.9.9 (Something Else)'\n(sleep 4) &\nexit 0\n",
		"no output":            "(sleep 4) &\nexit 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			version, err := claudeBuild(context.Background(), writeFakeClaude(t, body))
			if err == nil {
				t.Fatalf("version %q was accepted", version)
			}
		})
	}
}
