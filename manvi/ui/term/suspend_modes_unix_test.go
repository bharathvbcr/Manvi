//go:build unix

package term

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

const (
	beforeMarker  = "<<before>>"
	suspendMarker = "<<suspended>>"
)

// suspendRoundTrip starts a terminal over a pty with opts, suspends and resumes
// it, and returns what was written by Start, by the suspend up to the stop, and
// after the resume (which ends with Stop's own sequences).
func suspendRoundTrip(t *testing.T, opts Options) (start, suspended, resumed string) {
	t.Helper()
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	master, slave := openPTY(t)
	written := drainPTY(master)

	term := New(slave, slave)
	if err := term.Start(opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Stop() })

	// The marker is written from inside Suspend, after the restore sequences and
	// before the stop, so the byte stream itself says where the suspend ended.
	afterRestoreForTest = func() { _, _ = slave.WriteString(suspendMarker) }
	t.Cleanup(func() { afterRestoreForTest = nil })

	resume := exec.CommandContext(t.Context(), "/bin/sh", "-c", fmt.Sprintf("sleep 1; kill -CONT %d", syscall.Getpid())) // #nosec G204 -- the command line is built here from this test's own pid, not from input
	if err := resume.Start(); err != nil {
		t.Fatalf("starting the resume helper: %v", err)
	}
	defer func() {
		if err := resume.Wait(); err != nil {
			t.Errorf("the resume helper failed: %v", err)
		}
	}()
	_, _ = slave.WriteString(beforeMarker)
	if err := term.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)

	before, mark := strings.Index(all, beforeMarker), strings.Index(all, suspendMarker)
	if before < 0 || mark < before {
		t.Fatalf("the markers did not arrive in order: %q", all)
	}
	return all[:before], all[before+len(beforeMarker) : mark], all[mark+len(suspendMarker):]
}

func TestSuspendDisablesAndResumeRestoresEveryReportingModeThatWasOn(t *testing.T) {
	start, suspended, resumed := suspendRoundTrip(t, Options{
		AltScreen: true, Mouse: true, Motion: true, BracketedPaste: true, Focus: true,
	})
	for _, on := range []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1004h", "\x1b[?1006h", "\x1b[?2004h"} {
		if !strings.Contains(start, on) {
			t.Fatalf("Start did not enable %q; the test proves nothing: %q", on, start)
		}
	}
	for _, off := range []string{"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1004l", "\x1b[?1006l", "\x1b[?2004l"} {
		if !strings.Contains(suspended, off) {
			t.Errorf("a suspend left %q unsent, so the shell receives that reporting as input: %q", off, suspended)
		}
	}
	for _, on := range []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1004h", "\x1b[?1006h", "\x1b[?2004h"} {
		if !strings.Contains(resumed, on) {
			t.Errorf("a resume did not re-enable %q: %q", on, resumed)
		}
	}
}

func TestSuspendAndResumeTouchOnlyTheModesThatWereOn(t *testing.T) {
	_, suspended, resumed := suspendRoundTrip(t, Options{AltScreen: true, Mouse: true})
	for _, mode := range []string{"1003", "1004", "2004"} {
		if strings.Contains(suspended, "\x1b[?"+mode+"l") {
			t.Errorf("a suspend disabled mode %s, which was never on: %q", mode, suspended)
		}
		if strings.Contains(resumed, "\x1b[?"+mode+"h") {
			t.Errorf("a resume enabled mode %s, which was never on: %q", mode, resumed)
		}
	}
	if !strings.Contains(resumed, "\x1b[?1000h\x1b[?1002h\x1b[?1006h") {
		t.Errorf("a resume did not restore the mouse modes that were on: %q", resumed)
	}
}
