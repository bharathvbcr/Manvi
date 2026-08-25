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
