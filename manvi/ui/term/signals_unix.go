//go:build unix

package term

import (
	"os"
	"os/signal"
	"syscall"
)

// resizeSignal is the signal a terminal sends when its window changes size.
func resizeSignal() os.Signal { return syscall.SIGWINCH }

// suspendSignals are the stop/continue pair behind Ctrl+Z.
func suspendSignals() (stop, cont os.Signal) { return syscall.SIGTSTP, syscall.SIGCONT }

// raiseSelf re-sends a signal to this process with the handler removed, which
// is how a program suspends itself for real.
//
// Doing it any other way — calling the default handler, or just not handling
// SIGTSTP — is what makes a TUI that either cannot be suspended or is suspended
// with the terminal still in raw mode and the alternate screen still active.
// The shell that regains the terminal in that state shows no prompt and no
// echo, and the user's next move is usually to kill the window.
func raiseSelf(sig os.Signal) error {
	signal.Reset(sig)
	s, ok := sig.(syscall.Signal)
	if !ok {
		return nil
	}
	return syscall.Kill(syscall.Getpid(), s)
}
