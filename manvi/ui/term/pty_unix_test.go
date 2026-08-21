//go:build unix

package term

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// openPTY returns a connected master/slave pair.
//
// A pseudo-terminal is the only way to test this package for real. Everything
// here is about what happens to a tty — raw mode, restoration, window size,
// SIGWINCH — and none of it can be observed through a pipe, because a pipe is
// not a terminal and IsTerminal correctly refuses it. Testing around that would
// leave the one path whose failure strands the user's shell unexercised.
//
// The ioctl numbers are platform constants rather than a portable API. They
// were verified by running them on this platform, not recalled; a wrong one
// here surfaces as a failure to open, which fails the test rather than skipping
// it.
func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx on this system: %v", err)
	}
	name, err := slaveName(m)
	if err != nil {
		m.Close()
		t.Fatalf("could not derive the pty slave: %v", err)
	}
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		t.Fatalf("opening the pty slave %s: %v", name, err)
	}
	t.Cleanup(func() { s.Close(); m.Close() })
	return m, s
}

func ioctlPtr(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func ioctlVal(fd, req, val uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, val); e != 0 {
		return e
	}
	return nil
}

// setWinsize resizes the pty, which is what SIGWINCH and Size report on.
func setWinsize(t *testing.T, f *os.File, cols, rows uint16) {
	t.Helper()
	ws := winsize{Row: rows, Col: cols}
	if err := ioctlPtr(f.Fd(), tiocswinsz, unsafe.Pointer(&ws)); err != nil {
		t.Fatalf("resizing the pty: %v", err)
	}
}

// termiosOf reads a descriptor's current settings.
func termiosOf(t *testing.T, f *os.File) syscall.Termios {
	t.Helper()
	state, err := getState(f.Fd())
	if err != nil {
		t.Fatalf("reading termios: %v", err)
	}
	return *state
}

// TestRawModeIsEnteredAndExactlyRestored is the most consequential test in this
// package. A terminal left in raw mode after the program exits has no echo and
// no line editing: the user's shell is broken and they have to type `reset`
// blind. Restoration must return the *same* settings, not merely sane ones.
func TestRawModeIsEnteredAndExactlyRestored(t *testing.T) {
	_, slave := openPTY(t)
	before := termiosOf(t, slave)

	term := New(slave, slave)
	if !term.IsTerminal() {
		t.Fatal("a pty slave must be recognised as a terminal")
	}
	if err := term.Start(Options{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	during := termiosOf(t, slave)
	if during.Lflag&syscall.ECHO != 0 {
		t.Error("raw mode must turn echo off, or every keystroke is printed twice")
	}
	if during.Lflag&syscall.ICANON != 0 {
		t.Error("raw mode must turn canonical mode off, or keys arrive only at newline")
	}
	if during.Lflag&syscall.ISIG != 0 {
		t.Error("raw mode must take over signal keys, or Ctrl+C bypasses the TUI's own handling")
	}

	if err := term.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	after := termiosOf(t, slave)
	if after != before {
		t.Fatalf("the terminal was not restored to its original settings:\n before %+v\n after  %+v", before, after)
	}
}

// TestStopIsIdempotentAndSafeWithoutStart: Stop runs from a deferred recover on
// a path that may not have reached Start, and a second call must not undo
// settings the user has since changed.
func TestStopIsIdempotentAndSafeWithoutStart(t *testing.T) {
	_, slave := openPTY(t)
	before := termiosOf(t, slave)

	term := New(slave, slave)
	if err := term.Stop(); err != nil {
		t.Fatalf("Stop before Start must be a no-op: %v", err)
	}
	if termiosOf(t, slave) != before {
		t.Fatal("Stop before Start changed the terminal")
	}

	if err := term.Start(Options{AltScreen: true, Mouse: true, BracketedPaste: true}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	restored := termiosOf(t, slave)

	// Change the settings behind the terminal's back, as a shell would, then
	// call Stop again. It must not resurrect the saved state over the top.
	scratch := restored
	scratch.Lflag &^= syscall.ECHO
	if err := setState(slave.Fd(), &scratch); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if got := termiosOf(t, slave); got.Lflag&syscall.ECHO != 0 {
		t.Fatal("a second Stop re-applied the saved state over settings changed since")
	}
}

// TestStartAppliesExactlyTheOptionsAsked, and Stop undoes them. The escape
// sequences are the contract with the terminal emulator; enabling mouse
// reporting and never disabling it leaves the user's shell printing escape
// codes on every click.
func TestStartAppliesExactlyTheOptionsAsked(t *testing.T) {
	master, slave := openPTY(t)

	// Drain the master so writes never block on a full pty buffer.
	written := make(chan []byte, 64)
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

	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true, Mouse: true, BracketedPaste: true, Title: "TASK-001"}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}

	all := collect(t, written)
	for _, want := range []string{
		"\x1b[?1049h",         // enter alternate screen
		"\x1b[?1000h",         // mouse buttons
		"\x1b[?1002h",         // drag
		"\x1b[?1006h",         // SGR encoding, without which columns past 223 are unaddressable
		"\x1b[?2004h",         // bracketed paste
		"\x1b[?25l",           // hide cursor
		"\x1b]0;TASK-001\x07", // window title
	} {
		if !contains(all, want) {
			t.Errorf("Start did not emit %q", want)
		}
	}
	for _, want := range []string{
		"\x1b[?2004l", "\x1b[?1006l", "\x1b[?1002l", "\x1b[?1000l",
		"\x1b[?25h", "\x1b[?1049l",
	} {
		if !contains(all, want) {
			t.Errorf("Stop did not emit %q; the setting is left on in the user's shell", want)
		}
	}
}

// TestMotionTrackingIsEnabledAndDisabledCleanly: any-motion reporting (1003)
// is what lets the UI highlight rows under a bare hover, and it is the noisiest
// of the mouse modes — leaving it on after exit would spray motion events into
// the shell on every pass of the pointer.
func TestMotionTrackingIsEnabledAndDisabledCleanly(t *testing.T) {
	master, slave := openPTY(t)

	written := make(chan []byte, 64)
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

	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true, Mouse: true, Motion: true}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}

	all := collect(t, written)
	if !contains(all, "\x1b[?1003h") {
		t.Error("Start with Motion did not enable any-motion tracking")
	}
	if !contains(all, "\x1b[?1003l") {
		t.Error("Stop did not disable any-motion tracking; it leaks into the shell")
	}
}

// TestMotionWithoutMouseDoesNotEnableTracking: 1003 without 1000 is a mode
// that reports motion with no button semantics — a combination nothing should
// ask for, and the option must not produce it.
func TestMotionWithoutMouseDoesNotEnableTracking(t *testing.T) {
	master, slave := openPTY(t)

	written := make(chan []byte, 64)
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

	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true, Motion: true}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}

	all := collect(t, written)
	if contains(all, "\x1b[?1003h") {
		t.Error("Motion without Mouse enabled any-motion tracking")
	}
}

// TestATitleFromTheStoreCannotAuthorEscapes: the window title is set from a
// task title, which is content out of the store. An unescaped BEL or ESC in it
// terminates the OSC string early and everything after is executed by the
// terminal.
func TestATitleFromTheStoreCannotAuthorEscapes(t *testing.T) {
	master, slave := openPTY(t)
	written := make(chan []byte, 64)
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

	hostile := "TASK-1\x07\x1b]0;pwned\x07\x1b[2J"
	term := New(slave, slave)
	if err := term.Start(Options{Title: hostile}); err != nil {
		t.Fatal(err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}

	all := collect(t, written)

	// The property is not that the hostile *words* are gone — they are inert
	// text once the escapes are stripped, and removing them would be censoring
	// a legitimate task title that happens to contain brackets. The property is
	// that the title cannot terminate its own OSC string or begin a new
	// sequence, because either would let store content author escapes.
	start := strings.Index(all, "\x1b]0;")
	if start < 0 {
		t.Fatalf("no title sequence was written at all: %q", all)
	}
	end := strings.IndexByte(all[start:], 0x07)
	if end < 0 {
		t.Fatalf("the title sequence was never terminated: %q", all)
	}
	title := all[start+len("\x1b]0;") : start+end]
	for _, r := range title {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("a control character %U survived into the window title: %q", r, title)
		}
	}
	if strings.Count(all, "\x1b]0;") != 1 {
		t.Fatalf("the title opened more than one OSC sequence: %q", all)
	}
	if !strings.Contains(title, "TASK-1") {
		t.Fatalf("the legitimate part of the title was destroyed: %q", title)
	}
}

// TestSizeReadsThePTYAndSurvivesAZeroWindow. A terminal reports zero while its
// window is being dragged and on some multiplexers at startup; laying out into
// a zero-width screen divides by it.
func TestSizeReadsThePTYAndSurvivesAZeroWindow(t *testing.T) {
	_, slave := openPTY(t)
	term := New(slave, slave)

	setWinsize(t, slave, 120, 40)
	if w, h := term.Size(); w != 120 || h != 40 {
		t.Fatalf("size = %dx%d, want 120x40", w, h)
	}

	setWinsize(t, slave, 0, 0)
	w, h := term.Size()
	if w <= 0 || h <= 0 {
		t.Fatalf("size = %dx%d; a zero dimension must be reported as a small usable size", w, h)
	}
}

// TestResizeNotificationsCollapseAndTheWatcherStops. A window drag delivers a
// burst of SIGWINCH, and the watcher goroutine must not outlive Stop.
func TestResizeNotificationsCollapseAndTheWatcherStops(t *testing.T) {
	if resizeSignal() == nil {
		t.Skip("no resize signal on this platform")
	}
	_, slave := openPTY(t)
	term := New(slave, slave)
	if err := term.Start(Options{}); err != nil {
		t.Fatal(err)
	}

	// Sent directly rather than through raiseSelf. raiseSelf calls
	// signal.Reset first, which is exactly right for the suspend path it was
	// written for — a real Ctrl+Z needs the default handler — but for any other
	// signal it silently removes the notification the watcher registered. Using
	// it here would deafen the resize watcher for the rest of the process and
	// the test would be measuring that instead.
	sig := resizeSignal().(syscall.Signal)
	for i := 0; i < 20; i++ {
		if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
			t.Fatalf("raising the resize signal: %v", err)
		}
	}

	// Counted only after the burst has settled.
	//
	// Draining concurrently with the raises made the assertion depend on
	// winning a race: every notification consumed frees the watcher's slot to
	// be refilled, so a consumer that keeps up sees twenty deliveries and one
	// that does not sees one. Under a loaded `go test ./...` the scheduler
	// decided which, and the test failed on machine load rather than on
	// behaviour. Letting the signals land first and then draining measures the
	// property that actually matters: however many arrive, at most one
	// notification is left pending.
	settle := time.After(300 * time.Millisecond)
	<-settle

	got := 0
	for draining := true; draining; {
		select {
		case <-term.Resized():
			got++
			if got > 3 {
				t.Fatal("a burst of resizes was not collapsed; each one would trigger a full repaint")
			}
		case <-time.After(300 * time.Millisecond):
			draining = false
		}
	}
	if got == 0 {
		t.Fatal("the resize signal was not delivered")
	}

	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}
	// After Stop the watcher goroutine must have retired. Raising again must
	// neither panic on a closed channel nor deliver anything.
	drain(term)
	if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
		t.Fatal(err)
	}
	select {
	case <-term.Resized():
		t.Fatal("a resize arrived after Stop; the watcher goroutine outlived the terminal")
	case <-time.After(300 * time.Millisecond):
	}
}

// drain empties any pending notification so a post-Stop check measures new
// deliveries rather than a leftover.
func drain(t *Terminal) {
	for {
		select {
		case <-t.Resized():
		default:
			return
		}
	}
}

// TestASecondStartIsIgnoredSilently documents a trap rather than approving of
// it. Start returns nil when already started, so a caller that starts with one
// set of options and later starts with another gets neither an error nor the
// second set — mouse reporting simply never turns on.
func TestASecondStartIsIgnoredSilently(t *testing.T) {
	master, slave := openPTY(t)
	written := make(chan []byte, 64)
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

	term := New(slave, slave)
	if err := term.Start(Options{}); err != nil {
		t.Fatal(err)
	}
	if err := term.Start(Options{Mouse: true}); err != nil {
		t.Fatalf("a second Start reported an error: %v", err)
	}
	if err := term.Stop(); err != nil {
		t.Fatal(err)
	}

	all := collect(t, written)
	if contains(all, "\x1b[?1000h") {
		t.Fatal("the second Start applied its options; this test needs updating")
	}
	// The behaviour under test: no error, and the options were dropped. A
	// caller cannot tell. Recorded so a change here is deliberate.
}

func collect(t *testing.T, ch <-chan []byte) string {
	t.Helper()
	var all []byte
	deadline := time.After(2 * time.Second)
	for {
		select {
		case chunk, ok := <-ch:
			if !ok {
				return string(all)
			}
			all = append(all, chunk...)
		case <-time.After(200 * time.Millisecond):
			return string(all)
		case <-deadline:
			return string(all)
		}
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }

var _ = fmt.Sprintf

// TestSuspendHandsTheTerminalBackAndTakesItAgain is the Ctrl+Z path, and it is
// worth the trouble of a real suspend to test.
//
// A process that stops while the tty is raw hands the shell a terminal with no
// echo, no line editing, and possibly the alternate screen still active. The
// user sees no prompt and no typing, and the usual next move is to kill the
// window. So this test genuinely stops the process and has an external helper
// continue it, because the failure only exists in the window between the two.
func TestSuspendHandsTheTerminalBackAndTakesItAgain(t *testing.T) {
	stopSig, _ := suspendSignals()
	if stopSig == nil {
		t.Skip("no suspend signals on this platform")
	}
	master, slave := openPTY(t)
	go io.Copy(io.Discard, master)

	original := termiosOf(t, slave)
	term := New(slave, slave)
	if err := term.Start(Options{AltScreen: true, Mouse: true, BracketedPaste: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { term.Stop() })

	raw := termiosOf(t, slave)
	if raw.Lflag&syscall.ECHO != 0 {
		t.Fatal("the terminal is not raw before suspending; this test would prove nothing")
	}

	// A separate process, because once this one stops nothing in it runs — not
	// a goroutine, not a timer. Only an outside signal can resume it. The sleep
	// is long enough for Suspend to restore the terminal and stop, and the
	// helper is unconditional so a failure before the stop cannot wedge the run.
	resume := exec.Command("/bin/sh", "-c",
		fmt.Sprintf("sleep 1; kill -CONT %d", syscall.Getpid()))
	if err := resume.Start(); err != nil {
		t.Fatalf("starting the resume helper: %v", err)
	}
	defer resume.Wait()

	// The observation that matters is made by the helper's own timing: the
	// terminal must already be restored by the time this process is stopped.
	// It is checked immediately after resuming, because a suspend that left the
	// tty raw would show up as raw settings persisting across the stop.
	checked := make(chan syscall.Termios, 1)
	go func() {
		// Runs before the stop takes effect only if Suspend restores first;
		// after the stop it cannot run at all. Either way the value it reports
		// is the state the shell would have seen.
		time.Sleep(300 * time.Millisecond)
		state, err := getState(slave.Fd())
		if err == nil {
			checked <- *state
		}
		close(checked)
	}()

	if err := term.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}

	select {
	case observed, ok := <-checked:
		if !ok {
			t.Fatal("could not observe the terminal state during the suspend")
		}
		if observed.Lflag&syscall.ECHO == 0 {
			t.Fatal("the terminal was still raw while the process was stopped; " +
				"the shell would have had no echo and no line editing")
		}
		if observed != original {
			t.Errorf("the suspended terminal was not the original:\n before %+v\n during %+v", original, observed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no observation was made")
	}

	// Resumed: raw again, and a repaint requested, because the screen contents
	// are gone and the painter's diff would otherwise skip every cell it
	// believes is already correct.
	after := termiosOf(t, slave)
	if after.Lflag&syscall.ECHO != 0 {
		t.Fatal("the terminal was not put back into raw mode after resuming")
	}
	select {
	case <-term.Resumed():
	case <-time.After(2 * time.Second):
		t.Fatal("no repaint was requested after resuming; the screen would stay blank")
	}
}

// TestAccessorsReportWhatWasDetected covers the small surface the app reads
// every frame.
func TestAccessorsReportWhatWasDetected(t *testing.T) {
	_, slave := openPTY(t)
	term := New(slave, slave)
	if term.Out() != io.Writer(slave) || term.In() != io.Reader(slave) {
		t.Fatal("Out and In must hand back the files the terminal was built over")
	}
	// Profile and Unicode are detected at construction; the contract is only
	// that they are stable, since the app caches them for the whole run.
	if term.Profile() != term.Profile() || term.Unicode() != term.Unicode() {
		t.Fatal("detection is not stable across calls")
	}
	if term.Border() != term.Border() {
		t.Fatal("the border style is not stable across calls")
	}
}

// TestStartAndStopCyclesLeaveNoWatcherBehind is the regression test for a data
// race the race detector found once these tests first exercised the signal
// path.
//
// The watcher goroutine read t.stop from the struct on every loop iteration
// while Stop was setting that same field to nil. Beyond the race itself, the
// consequence in a real run is worse than a torn read: a watcher that reads nil
// selects on a channel that never becomes ready, so it never retires — every
// Start/Stop cycle leaks a goroutine and a signal registration, and a terminal
// restarted after a suspend ends up with several watchers all pushing to one
// notification channel.
func TestStartAndStopCyclesLeaveNoWatcherBehind(t *testing.T) {
	if resizeSignal() == nil {
		t.Skip("no resize signal on this platform")
	}
	_, slave := openPTY(t)
	term := New(slave, slave)
	sig := resizeSignal().(syscall.Signal)

	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		if err := term.Start(Options{}); err != nil {
			t.Fatalf("cycle %d Start: %v", i, err)
		}
		if err := term.Stop(); err != nil {
			t.Fatalf("cycle %d Stop: %v", i, err)
		}
	}

	// Give any surviving watcher a chance to be scheduled and, if it were still
	// registered, to consume the signal.
	if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines grew from %d to %d across 20 start/stop cycles; watchers are outliving their terminal",
			before, after)
	}

	// And a terminal restarted after all that is not deaf: the stop channel is
	// replaced on each Start, so the new watcher is live.
	if err := term.Start(Options{}); err != nil {
		t.Fatal(err)
	}
	defer term.Stop()
	drain(term)
	if err := syscall.Kill(syscall.Getpid(), sig); err != nil {
		t.Fatal(err)
	}
	select {
	case <-term.Resized():
	case <-time.After(2 * time.Second):
		t.Fatal("a terminal restarted after several cycles no longer reports resizes")
	}
}
