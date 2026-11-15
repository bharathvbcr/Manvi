//go:build unix

package term

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// drainPTY collects everything written to the terminal side of a pty.
func drainPTY(master *os.File) <-chan []byte {
	written := make(chan []byte, 256)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				written <- chunk
			}
			if err != nil {
				close(written)
				return
			}
		}
	}()
	return written
}

// statusEnv pins every variable the title and progress rules read, so a test
// describes one terminal whatever the machine running it is.
func statusEnv(t *testing.T, pairs ...string) {
	t.Helper()
	for _, k := range []string{
		"MANVI_TERM_TITLE", "MANVI_TERM_PROGRESS", "TMUX", "STY",
		"WT_SESSION", "ConEmuANSI", "TERM_PROGRAM", "TERM_PROGRAM_VERSION",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("TERM", "xterm-256color")
	for i := 0; i+1 < len(pairs); i += 2 {
		t.Setenv(pairs[i], pairs[i+1])
	}
}

const (
	liveTitle = "thinking · session one — manvi"
	busySeq   = "\x1b]9;4;3;0\x07"
	clearSeq  = "\x1b]9;4;0;0\x07"
)

// The caller asks on every frame and the terminal writes only when the answer
// has changed. Fifty frames of one state are one title and one progress
// sequence: a title rewritten per frame is a redraw loop that never rests, and
// the reason this is not left to the caller is that the next caller would
// forget.
func TestTitleAndProgressAreWrittenOnlyWhenTheyChange(t *testing.T) {
	statusEnv(t, "WT_SESSION", "0c1d")
	master, slave := openPTY(t)
	written := drainPTY(master)

	term := New(slave, slave)
	if err := term.Start(Options{Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	for range 50 {
		term.SetTitle(liveTitle)
		term.SetProgress(ProgressBusy)
	}
	term.SetTitle("done · session one — manvi")
	term.SetProgress(ProgressNone)
	// Back to a state already seen: that is a change, and is written.
	term.SetTitle(liveTitle)
	term.SetProgress(ProgressBusy)
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)

	if n := strings.Count(all, titleSequence(liveTitle)); n != 2 {
		t.Fatalf("the running title was written %d times, want 2 (once per time it became true): %q", n, all)
	}
	if n := strings.Count(all, busySeq); n != 2 {
		t.Fatalf("the busy indicator was written %d times, want 2: %q", n, all)
	}
	if n := strings.Count(all, "\x1b]0;"); n != 5 {
		// start, running, done, running, and the plain title put back at Stop.
		t.Fatalf("%d title sequences were written, want 5: %q", n, all)
	}
}

// What is written is taken back. Stopped in the middle of a run, the terminal
// is left with no progress showing and the plain title on its tab, written
// before the title stack is popped so a terminal with no stack is not left
// saying an agent is running at a shell prompt.
func TestStopClearsTheProgressAndPutsThePlainTitleBack(t *testing.T) {
	statusEnv(t, "WT_SESSION", "0c1d")
	master, slave := openPTY(t)
	written := drainPTY(master)

	term := New(slave, slave)
	if err := term.Start(Options{Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	term.SetTitle(liveTitle)
	term.SetProgress(ProgressError)
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	// After Stop nothing more is written, and a nil terminal is a no-op.
	term.SetTitle("late")
	term.SetProgress(ProgressBusy)
	var none *Terminal
	none.SetTitle("x")
	none.SetProgress(ProgressBusy)
	all := collect(t, written)

	live := strings.LastIndex(all, titleSequence(liveTitle))
	plain := strings.LastIndex(all, titleSequence("manvi"))
	cleared := strings.LastIndex(all, clearSeq)
	pop := strings.LastIndex(all, "\x1b[23;0t")
	if live < 0 || plain < live {
		t.Fatalf("the plain title was not written after the live one: %q", all)
	}
	if cleared < strings.LastIndex(all, "\x1b]9;4;2;100\x07") {
		t.Fatalf("the error indicator was not cleared after it was set: %q", all)
	}
	if pop < plain || pop < cleared {
		t.Fatalf("the title stack was popped before the terminal was settled: %q", all)
	}
	if strings.Contains(all, "late") || strings.Count(all, busySeq) != 0 {
		t.Fatalf("something was written after Stop: %q", all)
	}

	// A run that never changed either writes nothing extra on the way out.
	statusEnv(t, "WT_SESSION", "0c1d")
	master, slave = openPTY(t)
	written = drainPTY(master)
	quiet := New(slave, slave)
	if err := quiet.Start(Options{Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	if err := quiet.Stop(); err != nil {
		t.Fatal(err)
	}
	all = collect(t, written)
	if strings.Count(all, "\x1b]0;") != 1 || strings.Contains(all, "\x1b]9;4") {
		t.Fatalf("an untouched run wrote more than its starting title: %q", all)
	}
}

// A terminal nothing is known about gets no progress bytes at all, however
// often it is asked; and inside GNU screen the title stays the one written at
// the start.
func TestUnknownTerminalsGetNoProgressAndScreenKeepsAStaticTitle(t *testing.T) {
	statusEnv(t)
	master, slave := openPTY(t)
	written := drainPTY(master)
	term := New(slave, slave)
	if err := term.Start(Options{Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []Progress{ProgressBusy, ProgressPaused, ProgressError, ProgressNone} {
		term.SetProgress(p)
	}
	term.SetTitle(liveTitle)
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)
	if strings.Contains(all, "\x1b]9") {
		t.Fatalf("an unknown terminal was sent a progress sequence: %q", all)
	}
	if !strings.Contains(all, titleSequence(liveTitle)) {
		t.Fatalf("an ordinary terminal did not get the live title: %q", all)
	}

	statusEnv(t, "STY", "1234.pts-0.host", "WT_SESSION", "0c1d")
	master, slave = openPTY(t)
	written = drainPTY(master)
	inScreen := New(slave, slave)
	if err := inScreen.Start(Options{Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	inScreen.SetTitle(liveTitle)
	inScreen.SetProgress(ProgressBusy)
	if err := inScreen.Stop(); err != nil {
		t.Fatal(err)
	}
	all = collect(t, written)
	if strings.Count(all, "\x1b]0;") != 1 || strings.Contains(all, "\x1b]9") {
		t.Fatalf("inside GNU screen more than the starting title was written: %q", all)
	}
}

// A live title names a session and a tool, which is content. It must not be
// able to end its own sequence or begin another, in either control range.
func TestALiveTitleCannotAuthorEscapes(t *testing.T) {
	statusEnv(t)
	master, slave := openPTY(t)
	written := drainPTY(master)
	term := New(slave, slave)
	if err := term.Start(Options{Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	term.SetTitle("running rm\x07\x1b]0;pwned\x07\x1b[2J\u009c\u009d0;again · s — manvi")
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)
	// The starting title, the live one, and the plain one put back: three, and
	// the hostile text opened none of its own.
	if n := strings.Count(all, "\x1b]0;"); n != 3 {
		t.Fatalf("%d title sequences were opened, want 3: %q", n, all)
	}
	if strings.Contains(all, "\x1b[2J") || strings.ContainsAny(all, "\u009c\u009d") {
		t.Fatalf("a control sequence from the title reached the terminal: %q", all)
	}
	if !strings.Contains(all, "running rm]0;pwned[2J0;again") {
		t.Fatalf("the printable part of the title was lost: %q", all)
	}
}

// A suspend hands the terminal to a shell, so the tab must stop saying an
// agent is running, and the resume must say it again: the sequences are
// written a second time even though, as far as the caller knows, nothing
// changed.
func TestSuspendSettlesTheTabAndResumeWritesItAgain(t *testing.T) {
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	statusEnv(t, "WT_SESSION", "0c1d")
	master, slave := openPTY(t)
	written := drainPTY(master)

	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true, Title: "manvi"}); err != nil {
		t.Fatal(err)
	}
	term.SetTitle(liveTitle)
	term.SetProgress(ProgressBusy)

	// Only an outside process can continue this one once it has stopped.
	resume := exec.Command("/bin/sh", "-c", fmt.Sprintf("sleep 1; kill -CONT %d", syscall.Getpid()))
	if err := resume.Start(); err != nil {
		t.Fatalf("starting the resume helper: %v", err)
	}
	defer resume.Wait()
	if err := term.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	// The same answers as before the suspend, asked again by the first frame.
	term.SetTitle(liveTitle)
	term.SetProgress(ProgressBusy)
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	all := collect(t, written)

	first := strings.Index(all, titleSequence(liveTitle))
	second := strings.LastIndex(all, titleSequence(liveTitle))
	if first < 0 || second == first {
		t.Fatalf("the live title was not written again after the resume: %q", all)
	}
	between := all[first:second]
	if !strings.Contains(between, clearSeq) || !strings.Contains(between, titleSequence("manvi")) {
		t.Fatalf("the suspend did not clear the progress and put the plain title back: %q", between)
	}
	if strings.Count(all, busySeq) != 2 {
		t.Fatalf("the busy indicator was written %d times, want 2 (before and after the suspend): %q",
			strings.Count(all, busySeq), all)
	}
}
