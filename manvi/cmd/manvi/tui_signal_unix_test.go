//go:build linux || darwin

package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/ptytest"
)

const tuiHelperEnv = "MANVI_TUI_PTY_HELPER"

// TestTUIPtyHelper is the body of the re-executed binary: the same entry point
// `manvi tui` takes, on whatever terminal the parent gave it. It does nothing in
// an ordinary run.
func TestTUIPtyHelper(t *testing.T) {
	if os.Getenv(tuiHelperEnv) == "" {
		t.Skip("helper for the TUI pty tests")
	}
	os.Exit(exitStatus(os.Stdout, os.Stderr, []string{"tui"}))
}

// ptyOutput collects what the child writes to its terminal.
type ptyOutput struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (o *ptyOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}

func (o *ptyOutput) len() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Len()
}

func (o *ptyOutput) from(n int) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.buf.Bytes()[n:])
}

func (o *ptyOutput) contains(s string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return bytes.Contains(o.buf.Bytes(), []byte(s))
}

// tuiOnAPty starts `manvi tui` as its own session on a fresh pty, waits for it
// to take the screen, and returns the process, the pty ends and its output.
func tuiOnAPty(t *testing.T) (cmd *exec.Cmd, master, slave *os.File, out *ptyOutput) {
	t.Helper()
	work := t.TempDir()
	// #nosec G204 -- a literal git invocation in a test directory.
	if err := exec.CommandContext(t.Context(), "git", "init", "-q", work).Run(); err != nil {
		t.Skipf("git is needed for the project the TUI opens: %v", err)
	}
	master, slave = ptytest.Open(t)
	ptytest.SetWinsize(t, slave, 100, 30)

	// #nosec G204,G702 -- re-executes this test binary with a literal -test.run.
	cmd = exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestTUIPtyHelper$")
	cmd.Dir = work
	cmd.Env = append(os.Environ(), tuiHelperEnv+"=1", "HOME="+t.TempDir(),
		"TERM=xterm-256color", "COLORTERM=truecolor", "LANG=en_US.UTF-8")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the TUI: %v", err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	out = &ptyOutput{}
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				_, _ = out.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(60 * time.Second)
	for !out.contains("\x1b[?1049h") {
		if time.Now().After(deadline) {
			t.Fatalf("the TUI never took the alternate screen; it wrote %d bytes", out.len())
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cmd, master, slave, out
}

// waitExit waits for the process to end and returns its error (nil is status 0).
func waitExit(t *testing.T, cmd *exec.Cmd, within time.Duration) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("the TUI was still running %s after it was told to stop", within)
		return nil
	}
}

// restoredAfter checks everything a stopped TUI owes the terminal it took.
func restoredAfter(t *testing.T, slave *os.File, out *ptyOutput, mark int) {
	t.Helper()
	// The sequences are written just before the process exits; give the pty
	// reader a moment to deliver them.
	want := map[string]string{
		"alternate screen": "\x1b[?1049l",
		"mouse reporting":  "\x1b[?1000l",
		"bracketed paste":  "\x1b[?2004l",
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		tail := out.from(mark)
		missing := ""
		for name, seq := range want {
			if !strings.Contains(tail, seq) {
				missing = name
			}
		}
		if missing == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the terminal was not given back: %s still set after the stop\ntail: %q", missing, tail)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ptytest.Cooked(t, slave) {
		t.Fatal("the terminal was left in raw mode: echo or line editing is still off")
	}
}

// A TUI that is told to stop by its environment -- the window closing (SIGHUP),
// a supervisor (SIGTERM), or an interrupt that did not come through the keyboard
// (SIGINT; the terminal is raw, so Ctrl-C is a key) -- must leave the terminal as
// it found it and exit through the same path as a quit, which is what releases
// the sessions' leases and lets a running turn save its log. Without a handler
// the process died with the alternate screen, mouse reporting and bracketed
// paste still on and the tty raw.
func TestTUIStopSignalsRestoreTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  syscall.Signal
		said string
	}{
		{"SIGTERM", syscall.SIGTERM, "stopped by terminated"},
		{"SIGHUP", syscall.SIGHUP, "stopped by hangup"},
		{"SIGINT", syscall.SIGINT, "stopped by interrupt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, _, slave, out := tuiOnAPty(t)
			mark := out.len()
			if err := cmd.Process.Signal(tc.sig); err != nil {
				t.Fatalf("signalling the TUI: %v", err)
			}
			err := waitExit(t, cmd, 30*time.Second)
			var ee *exec.ExitError
			if !errors.As(err, &ee) {
				t.Fatalf("a stopped TUI exited with %v, want status 1", err)
			}
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				t.Fatalf("the TUI was killed by %v instead of shutting down", ws.Signal())
			}
			if ee.ExitCode() != 1 {
				t.Fatalf("exit status %d, want 1", ee.ExitCode())
			}
			restoredAfter(t, slave, out, mark)
			if tail := out.from(mark); !strings.Contains(tail, tc.said) {
				t.Fatalf("the stop was not reported (want %q): %q", tc.said, tail)
			}
		})
	}
}

// The control: the keyboard quit restores the terminal the same way, so the
// signal paths above are held to what a normal exit does.
func TestTUIQuitKeyRestoresTheTerminal(t *testing.T) {
	cmd, master, slave, out := tuiOnAPty(t)
	mark := out.len()
	// Ctrl-Q twice: the first arms the confirmation, the second quits.
	if _, err := master.Write([]byte{0x11}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := master.Write([]byte{0x11}); err != nil {
		t.Fatal(err)
	}
	if err := waitExit(t, cmd, 30*time.Second); err != nil {
		t.Fatalf("a keyboard quit exited with %v", err)
	}
	restoredAfter(t, slave, out, mark)
}

// cancelOnSignal ends the run on the first signal, says which one it was, and
// leaves later signals to their default action.
func TestCancelOnSignalCancelsAndNamesTheSignal(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received, release := cancelOnSignal(cancel, syscall.SIGHUP)
	defer release()
	if received() != nil {
		t.Fatal("a signal was reported before one arrived")
	}
	if err := syscall.Kill(os.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the run was not cancelled by the signal")
	}
	if got := received(); got != syscall.SIGHUP {
		t.Fatalf("reported %v, want hangup", got)
	}
}

// Releasing the handler before any signal leaves nothing registered and the
// run uncancelled.
func TestCancelOnSignalReleaseStopsListening(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	received, release := cancelOnSignal(cancel, syscall.SIGUSR2)
	release()
	release() // idempotent
	if received() != nil || ctx.Err() != nil {
		t.Fatal("a released handler acted")
	}
}

// A stop signal that arrives at any point before the run returns is reported,
// including one taken after the loop has ended, while leases are released; and
// the run's own failure is not hidden behind it.
func TestStoppedByNamesASignalThatArrivedAtAnyPoint(t *testing.T) {
	own := errors.New("the terminal went away")
	for _, tc := range []struct {
		name string
		sig  os.Signal
		run  error
		want string
	}{
		{"no signal, clean quit", nil, nil, ""},
		{"no signal, run failed", nil, own, own.Error()},
		{"signal, loop cancelled", syscall.SIGTERM, context.Canceled, "stopped by terminated"},
		{"signal after a clean loop exit, during release", syscall.SIGHUP, nil, "stopped by hangup"},
		{"signal, run failed for its own reason", syscall.SIGTERM, own, own.Error()},
	} {
		got := stoppedBy(tc.sig, tc.run)
		switch {
		case tc.want == "" && got != nil:
			t.Errorf("%s: got %v, want nil", tc.name, got)
		case tc.want != "" && (got == nil || got.Error() != tc.want):
			t.Errorf("%s: got %v, want %q", tc.name, got, tc.want)
		}
	}
}
