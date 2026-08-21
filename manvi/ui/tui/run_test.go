package tui

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// newCaptureRunner builds the minimum Runner captureStderr touches. It does not
// go through New, because New requires a real terminal and the stderr drain has
// nothing to do with one.
func newCaptureRunner() *Runner {
	return &Runner{
		actions: make(chan Action, 512),
		done:    make(chan struct{}),
	}
}

// awaitNotice waits for a notice whose text contains want.
func awaitNotice(t *testing.T, r *Runner, want string, within time.Duration) ActionNotice {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case act := <-r.actions:
			notice, ok := act.(ActionNotice)
			if !ok {
				continue
			}
			if strings.Contains(notice.Text, want) {
				return notice
			}
		case <-deadline:
			t.Fatalf("no notice containing %q arrived within %s", want, within)
		}
	}
}

// TestCaptureStderrSurvivesAnOversizedLine is the regression that matters most
// here: the drain must not be a one-shot. A single line longer than the reader's
// token limit used to end the goroutine, and everything written to stderr after
// it was lost with no diagnosis anywhere.
func TestCaptureStderrSurvivesAnOversizedLine(t *testing.T) {
	r := newCaptureRunner()
	restore := r.captureStderr()
	defer restore()

	// Comfortably past bufio.Scanner's 64 KiB default token limit.
	fmt.Fprintln(os.Stderr, strings.Repeat("x", 300_000))
	fmt.Fprintln(os.Stderr, "this line comes after the oversized one")

	awaitNotice(t, r, "this line comes after the oversized one", 5*time.Second)
}

// TestCaptureStderrReportsAnOversizedLineRatherThanDroppingIt asserts the
// oversized line is still delivered, truncated and marked, rather than silently
// discarded. A diagnostic too long to show is still a diagnostic.
func TestCaptureStderrReportsAnOversizedLineRatherThanDroppingIt(t *testing.T) {
	r := newCaptureRunner()
	restore := r.captureStderr()
	defer restore()

	fmt.Fprintln(os.Stderr, "HEAD"+strings.Repeat("y", 300_000)+"TAIL")

	notice := awaitNotice(t, r, "HEAD", 5*time.Second)
	if len(notice.Text) > maxNoticeBytes+256 {
		t.Fatalf("notice was not truncated: %d bytes", len(notice.Text))
	}
	if !strings.Contains(notice.Text, "truncated") {
		t.Fatalf("a truncated notice must say so, got %q", clip(notice.Text))
	}
	if strings.Contains(notice.Text, "TAIL") {
		t.Fatalf("the notice kept the whole oversized line")
	}
}

// TestCaptureStderrNeverBlocksTheWriter is the severe half. When the drain
// stops, nothing empties the pipe, its buffer fills, and the next write to
// os.Stderr blocks forever — wedging whichever goroutine was reporting a
// problem. Writes must keep completing no matter what came before them.
func TestCaptureStderrNeverBlocksTheWriter(t *testing.T) {
	r := newCaptureRunner()
	restore := r.captureStderr()
	defer restore()

	// Drain continuously; this test is about the writer, not the consumer.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-r.actions:
			case <-stop:
				return
			}
		}
	}()

	fmt.Fprintln(os.Stderr, strings.Repeat("z", 300_000))

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than any pipe buffer, so a stalled drain cannot be masked.
		for i := 0; i < 500; i++ {
			fmt.Fprintf(os.Stderr, "line %d\n", i)
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writing to stderr blocked: the drain stopped and the pipe filled")
	}
}

func clip(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:120] + "..."
}
