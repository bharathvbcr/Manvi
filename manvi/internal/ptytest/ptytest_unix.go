//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package ptytest

import (
	"os"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Open returns a connected master/slave pair, closed when the test ends. It
// skips the test where the system has no /dev/ptmx.
func Open(t testing.TB) (master, slave *os.File) {
	t.Helper()
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx on this system: %v", err)
	}
	name, err := slaveName(m)
	if err != nil {
		_ = m.Close()
		t.Fatalf("could not derive the pty slave: %v", err)
	}
	// #nosec G304 -- name is the slave path the kernel just reported for the pty this call opened.
	s, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = m.Close()
		t.Fatalf("opening the pty slave %s: %v", name, err)
	}
	t.Cleanup(func() { _ = s.Close(); _ = m.Close() })
	return m, s
}

// winsize is the kernel's TIOCSWINSZ payload.
type winsize struct {
	Row, Col       uint16
	Xpixel, Ypixel uint16
}

// SetWinsize resizes the pty, which is what SIGWINCH and a size query report on.
func SetWinsize(t testing.TB, f *os.File, cols, rows uint16) {
	t.Helper()
	ws := winsize{Row: rows, Col: cols}
	// #nosec G103 -- ioctl's third argument is an unsafe pointer; there is no typed form.
	if err := ioctlPtr(f.Fd(), tiocswinsz, unsafe.Pointer(&ws)); err != nil {
		t.Fatalf("resizing the pty: %v", err)
	}
}

// Cooked reports whether the terminal is in its ordinary line-editing state:
// canonical input with echo. A program that dies holding the terminal in raw
// mode leaves it false, and the shell that regains it shows nothing typed.
func Cooked(t testing.TB, f *os.File) bool {
	t.Helper()
	tio, err := getTermios(int(f.Fd()))
	if err != nil {
		t.Fatalf("reading termios: %v", err)
	}
	return tio.Lflag&unix.ICANON != 0 && tio.Lflag&unix.ECHO != 0
}

func ioctlPtr(fd, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}
