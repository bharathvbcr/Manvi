package devcouncil

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestRunShellReturnsWhenDescendantHoldsThePipe pins the fix for the tool call
// that never came back. `sleep 90 &` inherits stdout and keeps it open after
// sh exits; os/exec's copy goroutine blocks on EOF and Wait never returns
// unless WaitDelay cuts the pipes. Before that was set, one backgrounded
// descendant wedged exec_command past its own five-minute deadline.
//
// The pre-fix failure mode is a hang, so the guard is a duration ceiling: with
// the grace period at 5s this returns in single-digit seconds; without it the
// test runs out of its own patience first.
func TestRunShellReturnsWhenDescendantHoldsThePipe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()

	start := time.Now()
	out, code, timedOut, err := runShell(ctx, t.TempDir(), "echo started && sleep 90 & true")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("runShell error: %v", err)
	}
	if timedOut {
		t.Fatal("the deadline fired; only the pipe was being held")
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("expected foreground output, got %q", out)
	}
	if code != 0 {
		t.Fatalf("the foreground command succeeded; exit code %d", code)
	}
	if elapsed > 30*time.Second {
		t.Fatalf("a descendant holding the pipe wedged runShell for %v", elapsed)
	}
}

// TestRunShellTimeoutSurvivesHeldPipes: when the deadline fires while
// descendants still hold the stdio pipes, the call must come back as a timeout
// rather than hang. Pre-fix this exact shape never returned at all.
func TestRunShellTimeoutSurvivesHeldPipes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	start := time.Now()
	out, _, timedOut, err := runShell(ctx, t.TempDir(), "sleep 600 & sleep 600")
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("a timeout is not an execution error: %v", err)
	}
	if !timedOut {
		t.Fatal("expected the deadline to be reported")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("timeout path took %v; descendants are holding Wait hostage", elapsed)
	}
	if strings.Contains(out, "\x00") {
		t.Fatal("output contains NUL bytes")
	}
}

// TestRunShellReportsNonZeroExit: the ordinary paths must not have regressed.
func TestRunShellReportsNonZeroExit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out, code, timedOut, err := runShell(ctx, t.TempDir(), "echo boom >&2; exit 3")
	if err != nil || timedOut {
		t.Fatalf("err=%v timedOut=%v", err, timedOut)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if !strings.Contains(out, "boom") {
		t.Fatalf("stderr capture lost: %q", out)
	}
}
