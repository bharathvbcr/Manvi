package proc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunBoundedReturnsTheDeadlineWhenTheCallNeverDoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })
	start := time.Now()
	err, timedOut := RunBounded(ctx, func() error {
		<-blocked
		return nil
	})

	if !timedOut || err != nil {
		t.Fatalf("a call that never returns reported err=%v timedOut=%v", err, timedOut)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s to honour a 50ms deadline", elapsed)
	}
}

func TestRunBoundedPassesTheCallsOwnErrorThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := errors.New("exit status 2")
	err, timedOut := RunBounded(ctx, func() error { return want })
	if timedOut || !errors.Is(err, want) {
		t.Fatalf("got err=%v timedOut=%v, want %v and false", err, timedOut, want)
	}
}

func TestRunBoundedDoesNotReportSuccessAsATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err, timedOut := RunBounded(ctx, func() error { return nil })
	if timedOut || err != nil {
		t.Fatalf("a clean call reported err=%v timedOut=%v", err, timedOut)
	}
}

func TestRunBoundedPrefersTheAnswerItAlreadyHas(t *testing.T) {
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		done := make(chan error, 1)
		done <- nil
		err, timedOut := RunBounded(ctx, func() error { return <-done })
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		_ = timedOut
	}
}

// This source check makes the shared bound a repository invariant. A direct
// cmd.Run call only starts CommandContext's bound after fork succeeds, so a
// wedged Start can outlive the timeout printed by its caller.
func TestEverySubprocessBoundaryUsesTheBound(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for n, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, "cmd.Run()") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+":"+strings.TrimSpace(line)+" (line "+itoa(n+1)+")")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these calls bypass the process-wide wait bound:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// TestEverySubprocessBoundaryIsGroupIsolated is the companion invariant to the
// one above, and it exists because the bound alone was not enough.
//
// CommandContext kills the direct child and nothing else. A child that started
// its own children leaves them holding the inherited stdout pipe, os/exec's
// copy goroutine blocks on an EOF that never arrives, and Wait does not return
// — so the call outlives the deadline its caller printed. Group isolation is
// what closes that, and it had been applied at only two of the twelve places
// this repository execs something: the shell tool and the MCP client. The
// store, the verifier, the repo map, the searcher, the incumbent CLI, both git
// paths and the quick actions each ran a child the deadline could not fully
// reach.
//
// A lesson applied at some call sites is a lesson the next call site is
// written without, so it is asserted over all of them here.
func TestEverySubprocessBoundaryIsGroupIsolated(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	// Counted per file rather than by proximity. The configuration and the
	// construction are not always adjacent — in dc/store a fourteen-line
	// comment about WaitDelay sits between them, and it belongs there — so a
	// proximity window either rejects correct code or has to be widened until
	// it would accept a call belonging to some other command. What actually
	// matters is that no file execs more children than it isolates.
	//
	// Two shared helpers, and one package that still has its own. ConfigureGroup
	// is for a child bounded by one caller's deadline and arms cmd.Cancel;
	// ConfigureOwnGroup is for a child that outlives any single call — the MCP
	// servers and the store's serve sessions — and so is ended explicitly
	// rather than by a context. Both set Setpgid, which is what this test is
	// actually asserting. The shell tool in devcouncil keeps setOwnProcessGroup
	// because it merges into a SysProcAttr it also uses for other fields.
	//
	// A helper missing from this list reads exactly like a call site that never
	// isolated anything, so adding one here is part of adding one at all.
	configures := []string{
		"proc.ConfigureGroup(cmd)", "ConfigureGroup(cmd)",
		"proc.ConfigureOwnGroup(cmd)", "ConfigureOwnGroup(cmd)",
		"setOwnProcessGroup(cmd)",
	}

	var offenders []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		spawns, isolates := 0, 0
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if strings.Contains(trimmed, "exec.CommandContext(") {
				spawns++
			}
			for _, marker := range configures {
				if strings.Contains(trimmed, marker) {
					isolates++
					break
				}
			}
		}
		if spawns > isolates {
			rel, _ := filepath.Rel(root, path)
			offenders = append(offenders, rel+": "+itoa(spawns)+" child(ren) started, "+
				itoa(isolates)+" isolated")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("these children can outlive their deadline, because killing them "+
			"does not reach what they started:\n  %s", strings.Join(offenders, "\n  "))
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
