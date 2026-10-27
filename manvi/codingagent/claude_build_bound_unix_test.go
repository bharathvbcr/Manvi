//go:build unix

package codingagent

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The build probe is bounded by its deadline, not by whatever a descendant
// does with the stdout pipe. exec.CommandContext kills only the direct child,
// and Output then waits for the pipe to reach EOF: a `claude` that leaves a
// sleeper holding the pipe held the launch for the sleeper's whole life (a
// 10 s probe took 25 s in the audit). The caller's deadline reaches the probe
// too, so the test sets a short one rather than waiting out the real 10 s.
func TestClaudeBuildProbeEndsWithItsDeadlineWhileADescendantHoldsTheOutput(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "sleeper.pid")
	prog := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nsleep 25 &\necho $! > '" + pidFile + "'\nexec sleep 60\n"
	if err := os.WriteFile(prog, []byte(script), 0o700); err != nil { // #nosec G306 -- a script the test execs
		t.Fatal(err)
	}

	const deadline = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	started := time.Now()
	_, err := claudeBuild(ctx, prog)
	elapsed := time.Since(started)
	t.Logf("claudeBuild returned after %s: %v", elapsed.Round(10*time.Millisecond), err)

	if err == nil {
		t.Fatal("a probe that never printed its version was accepted")
	}
	// The deadline plus the grace period for pipes, with slack for a loaded
	// machine. The unbounded case is 25 s.
	if limit := deadline + claudeProbeWaitDelay + 2*time.Second; elapsed > limit {
		t.Fatalf("a probe with a %s deadline took %s (limit %s)", deadline, elapsed, limit)
	}

	// The deadline reaches the descendant too, not only the process it was
	// launched as: it is the thing holding the pipe, and it keeps running
	// against whatever it inherited.
	raw, readErr := os.ReadFile(pidFile) // #nosec G304 -- a file this test made under t.TempDir
	if readErr != nil {
		t.Fatalf("the fixture never recorded its sleeper: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if convErr != nil || pid <= 0 {
		t.Fatalf("sleeper pid %q: %v", raw, convErr)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("the descendant (pid %d) outlived the probe's deadline", pid)
		}
	}
}
