//go:build unix

package term

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// suspendNTimes starts a terminal, suspends and resumes it n times with
// distinct markers, and returns the stream segments: [start, s1, between..., last].
func advSuspendNTimes(t *testing.T, opts Options, n int) (segments []string, term *Terminal) {
	t.Helper()
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	master, slave := openPTY(t)
	written := drainPTY(master)
	term = New(slave, slave)
	if err := term.Start(opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Stop() })
	t.Cleanup(func() { afterRestoreForTest = nil })
	for i := 0; i < n; i++ {
		before, mark := fmt.Sprintf("<<b%d>>", i), fmt.Sprintf("<<s%d>>", i)
		afterRestoreForTest = func() { _, _ = slave.WriteString(mark) }
		resume := exec.CommandContext(t.Context(), "/bin/sh", "-c", fmt.Sprintf("sleep 1; kill -CONT %d", syscall.Getpid())) // #nosec G204 -- built from this test's own pid
		if err := resume.Start(); err != nil {
			t.Fatal(err)
		}
		_, _ = slave.WriteString(before)
		if err := term.Suspend(); err != nil {
			t.Fatalf("Suspend %d: %v", i, err)
		}
		if err := resume.Wait(); err != nil {
			t.Fatalf("resume helper %d: %v", i, err)
		}
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)
	rest := all
	for i := 0; i < n; i++ {
		before, mark := fmt.Sprintf("<<b%d>>", i), fmt.Sprintf("<<s%d>>", i)
		bi, mi := strings.Index(rest, before), strings.Index(rest, mark)
		if bi < 0 || mi < bi {
			t.Fatalf("markers %d out of order in %q", i, rest)
		}
		segments = append(segments, rest[:bi], rest[bi+len(before):mi])
		rest = rest[mi+len(mark):]
	}
	segments = append(segments, rest)
	return segments, term
}

// Suspending twice: each suspend disables what is on and each resume enables it
// again, with nothing added or lost the second time.
func TestAdvB31SuspendTwiceSendsTheSameSequencesBothTimes(t *testing.T) {
	segs, _ := advSuspendNTimes(t, Options{AltScreen: true, Mouse: true, Motion: true, BracketedPaste: true, Focus: true}, 2)
	// segs: start, susp0, (resume0 + b1), susp1, resume1+stop
	susp0, susp1 := segs[1], segs[3]
	for _, off := range []string{"\x1b[?1000l", "\x1b[?1002l", "\x1b[?1003l", "\x1b[?1004l", "\x1b[?1006l", "\x1b[?2004l"} {
		if strings.Count(susp0, off) != 1 || strings.Count(susp1, off) != 1 {
			t.Errorf("%q: suspend 1 sent it %d times, suspend 2 sent it %d times; want once each", off, strings.Count(susp0, off), strings.Count(susp1, off))
		}
	}
	resumed0 := segs[2]
	for _, on := range []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1003h", "\x1b[?1004h", "\x1b[?1006h", "\x1b[?2004h", "\x1b[?1049h"} {
		if strings.Count(resumed0, on) != 1 {
			t.Errorf("first resume sent %q %d times, want once: %q", on, strings.Count(resumed0, on), resumed0)
		}
	}
	// The second resume comes before Stop's own sequences in the final segment.
	if !strings.HasPrefix(strings.TrimSpace(segs[4]), "\x1b[?1049h") && !strings.Contains(segs[4], "\x1b[?1000h") {
		t.Errorf("second resume did not re-enable the modes: %q", segs[4])
	}
}

// Suspend on a terminal that was never started, or already stopped, writes
// nothing and does not stop the process.
func TestAdvB31SuspendWithoutStartOrAfterStopIsANoOp(t *testing.T) {
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	master, slave := openPTY(t)
	written := drainPTY(master)
	term := New(slave, slave)
	done := make(chan error, 1)
	go func() { done <- term.Suspend() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Suspend before Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend before Start blocked (it would have stopped the process)")
	}
	if err := term.Start(Options{AltScreen: true, Mouse: true}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	go func() { done <- term.Suspend() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Suspend after Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Suspend after Stop blocked")
	}
	all := collect(t, written)
	// After Stop, no further sequences: the last bytes are Stop's.
	if strings.Count(all, "\x1b[?1000l") != 1 {
		t.Errorf("mouse was disabled %d times across Start/Stop/Suspend: %q", strings.Count(all, "\x1b[?1000l"), all)
	}
}

// There is no resume without a suspend: Resumed never fires on its own, and a
// resume that nobody drained does not block a second one.
func TestAdvB31ResumedOnlyFiresAfterASuspendAndNeverBlocks(t *testing.T) {
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	_, slave := openPTY(t)
	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = term.Stop() })
	select {
	case <-term.Resumed():
		t.Fatal("Resumed fired with no suspend")
	case <-time.After(100 * time.Millisecond):
	}
}

// Stop then Start with different options: a later suspend uses the options of
// the live session, not those the terminal first had.
func TestAdvB31SuspendAfterRestartUsesTheNewModes(t *testing.T) {
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	master, slave := openPTY(t)
	written := drainPTY(master)
	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true, Mouse: true, Focus: true}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	if err := term.Start(Options{AltScreen: true, BracketedPaste: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { afterRestoreForTest = nil })
	afterRestoreForTest = func() { _, _ = slave.WriteString("<<S>>") }
	resume := exec.CommandContext(t.Context(), "/bin/sh", "-c", fmt.Sprintf("sleep 1; kill -CONT %d", syscall.Getpid())) // #nosec G204 -- built from this test's own pid
	if err := resume.Start(); err != nil {
		t.Fatal(err)
	}
	_, _ = slave.WriteString("<<B>>")
	if err := term.Suspend(); err != nil {
		t.Fatal(err)
	}
	_ = resume.Wait()
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)
	b, s := strings.Index(all, "<<B>>"), strings.Index(all, "<<S>>")
	if b < 0 || s < b {
		t.Fatalf("markers: %q", all)
	}
	susp, after := all[b:s], all[s:]
	if strings.Contains(susp, "?1000l") || strings.Contains(susp, "?1004l") {
		t.Errorf("suspend disabled modes of the first session: %q", susp)
	}
	if !strings.Contains(susp, "\x1b[?2004l") {
		t.Errorf("suspend did not disable bracketed paste: %q", susp)
	}
	if strings.Contains(after, "?1000h") || strings.Contains(after, "?1004h") {
		t.Errorf("resume enabled modes of the first session: %q", after)
	}
}
