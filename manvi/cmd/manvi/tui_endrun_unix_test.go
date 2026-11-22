//go:build unix

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// endRun cancels the run, waits for the refresh goroutine, and only then
// sweeps. Recorded in order, the three must appear as cancelled, returned,
// swept.
func TestEndRunCancelsAndWaitsForTheRefreshBeforeTheSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var (
		mu     sync.Mutex
		events []string
	)
	note := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		note("cancelled")
		time.Sleep(50 * time.Millisecond) // the build taking its time to stop
		note("refresh returned")
	}()
	problem := endRun(cancel, done, 5*time.Second, func() string {
		note("sweep")
		return ""
	})
	if problem != "" {
		t.Fatalf("a refresh that stopped in time was reported: %q", problem)
	}
	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(events, ","); got != "cancelled,refresh returned,sweep" {
		t.Fatalf("order = %q, want cancelled,refresh returned,sweep", got)
	}
}

// A refresh that never returns must not hold the quit: the wait ends at the
// bound, the sweep still runs, and the report says the refresh did not stop.
func TestEndRunBoundsAHungRefreshAndStillSweeps(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	never := make(chan struct{})
	swept := false
	start := time.Now()
	problem := endRun(cancel, never, 50*time.Millisecond, func() string { swept = true; return "sweep problem" })
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("a hung refresh held the quit for %v", d)
	}
	if !swept {
		t.Fatal("the sweep did not run after the wait timed out")
	}
	if !strings.Contains(problem, "did not stop") || !strings.Contains(problem, "sweep problem") {
		t.Fatalf("problem = %q, want both the hung refresh and the sweep's problem", problem)
	}
}

// The build child gets a catchable signal from the cancellation before the
// sweep's group SIGKILL arrives: a child that traps SIGTERM and records it gets
// to, because endRun does not sweep until the refresh has returned.
func TestEndRunLetsTheBuildChildSeeCancellationBeforeTheSweep(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "terminated")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// #nosec G204 -- a literal shell script; the marker path is a test temp file.
	build := exec.CommandContext(ctx, "sh", "-c", "trap 'echo seen > \"$1\"; exit 0' TERM; sleep 300 & wait", "sh", marker)
	build.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	build.Cancel = func() error { return syscall.Kill(-build.Process.Pid, syscall.SIGTERM) }
	build.WaitDelay = 10 * time.Second
	if err := build.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-build.Process.Pid, syscall.SIGKILL) })
	time.Sleep(300 * time.Millisecond) // let the shell install its trap
	done := make(chan struct{})
	go func() { defer close(done); _ = build.Wait() }()
	_ = endRun(cancel, done, 10*time.Second, endLeftoverChildren)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the build child was never told to stop before the sweep killed it: %v", err)
	}
}
