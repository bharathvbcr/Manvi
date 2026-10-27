//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package ptytest

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Verified by running them on this platform rather than recalled from memory.
const (
	tiocptygrant = 0x20007454
	tiocptyunlk  = 0x20007452
	tiocptygname = 0x40807453
	tiocswinsz   = 0x80087467
)

func slaveName(master *os.File) (string, error) {
	if err := ioctlVal(master.Fd(), tiocptygrant, 0); err != nil {
		return "", fmt.Errorf("granting the pty: %w", err)
	}
	if err := ioctlVal(master.Fd(), tiocptyunlk, 0); err != nil {
		return "", fmt.Errorf("unlocking the pty: %w", err)
	}
	var buf [128]byte
	// #nosec G103 -- ioctl's third argument is an unsafe pointer; there is no typed form.
	if err := ioctlPtr(master.Fd(), tiocptygname, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("reading the pty name: %w", err)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}

func ioctlVal(fd, req, val uintptr) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, val); e != 0 {
		return e
	}
	return nil
}

func getTermios(fd int) (*unix.Termios, error) { return unix.IoctlGetTermios(fd, unix.TIOCGETA) }
