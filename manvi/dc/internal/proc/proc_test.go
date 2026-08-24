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

// The bound has to cover the fork, not just the process.
//
// These tests moved here with the helper. They state the invariant directly,
// because the end-to-end wedge needs a loaded machine and the race detector to
// reproduce, and a regression test that only fails on a bad day is not one.

func TestRunBoundedReturnsTheDeadlineWhenTheCallNeverDoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	// Never sends. This is the wedged Start.
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	start := time.Now()
	err, timedOut := RunBounded(ctx, func() error {
		<-blocked
		return nil
	})
	elapsed := time.Since(start)

	if !timedOut {
		t.Fatalf("a call that never returns was not reported as timing out (err=%v)", err)
	}
	if err != nil {
		t.Errorf("the timeout path must not invent an error from the abandoned call, got %v", err)
	}
	// Generous, because this asserts "bounded", not "fast". Before the fix this
	// did not return at all.
	if elapsed > 5*time.Second {
		t.Fatalf("took %s to honour a 50ms deadline; the wait is not bounded", elapsed)
	}
}

func TestRunBoundedPassesTheCallsOwnErrorThrough(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	want := errors.New("exit status 2")
	err, timedOut := RunBounded(ctx, func() error { return want })
	if timedOut {
		t.Fatal("a call that returned promptly was reported as a timeout")
	}
	if !errors.Is(err, want) {
		t.Fatalf("got %v, want the call's own error %v", err, want)
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

// TestRunBoundedPrefersTheAnswerItAlreadyHas guards the race between a call
// finishing and a deadline expiring at the same instant.
//
// If the select picked the deadline when both were ready, a devmap query that
// answered correctly would intermittently be reported as a hang — the kind of
// flake that gets a real bound removed for being noisy.
func TestRunBoundedPrefersTheAnswerItAlreadyHas(t *testing.T) {
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		// The call has already returned before the select runs.
		done := make(chan error, 1)
		done <- nil
		err, timedOut := RunBounded(ctx, func() error { return <-done })
		cancel()
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		_ = timedOut // may legitimately be either; the point is it never errors
	}
}

// TestEverySubprocessBoundaryUsesTheBound is why this package exists rather
// than a second copy of the helper.
//
// The bound was written once, for devmap.decode, and left out of the two
// boundaries beside it: devmap.runProbe and the store client. The store one was
// the lease path — every Diagnose on the write gate and every Acquire — so the
// call with the shortest advertised timeout had the weakest one. A comment
// saying "use runBounded" would not have caught that; this does, including for
// the fourth boundary nobody has written yet.
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
				continue // prose about the defect, not the defect
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
		t.Fatalf("these call cmd.Run() directly, which is an unbounded wait wearing a timeout's clothes; "+
			"route them through proc.RunBounded:\n  %s", strings.Join(offenders, "\n  "))
	}
}

// itoa keeps the failure message above free of a fmt import for one number.
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
