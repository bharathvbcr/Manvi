//go:build unix

package term

import (
	"fmt"
	"syscall"
	"unsafe"
)

// winsize is the kernel's TIOCGWINSZ payload. It is declared here rather than
// taken from the standard library because the standard library does not export
// it; the layout is identical across the Unixes this file builds for.
type winsize struct {
	Row, Col       uint16
	Xpixel, Ypixel uint16
}

// state is the saved terminal configuration, opaque to the rest of the package
// so the non-Unix build can supply its own.
type state = syscall.Termios

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// getState reads the current terminal settings.
func getState(fd uintptr) (*syscall.Termios, error) {
	var t syscall.Termios
	if err := ioctl(fd, ioctlGetTermios, unsafe.Pointer(&t)); err != nil {
		return nil, fmt.Errorf("term: reading terminal state: %w", err)
	}
	return &t, nil
}

// setState writes terminal settings.
func setState(fd uintptr, t *syscall.Termios) error {
	if err := ioctl(fd, ioctlSetTermios, unsafe.Pointer(t)); err != nil {
		return fmt.Errorf("term: writing terminal state: %w", err)
	}
	return nil
}

// makeRaw puts the terminal into raw mode and returns the previous state.
//
// Raw mode is what makes a TUI possible: keystrokes arrive as they are typed
// rather than a line at a time, they are not echoed, and Ctrl+C arrives as a
// byte rather than as a signal the application never sees.
//
// That last property is the dangerous one. With ISIG cleared, nothing but this
// program can stop this program — so every path that enters raw mode must have
// a restore that runs even on a panic, and the input loop must handle 0x03
// itself. A harness that traps its operator inside a broken frame is worse than
// one that never drew it.
func makeRaw(fd uintptr) (*syscall.Termios, error) {
	prev, err := getState(fd)
	if err != nil {
		return nil, err
	}
	raw := *prev

	// Input: no CR-to-NL translation (so Enter is distinguishable from Ctrl+J),
	// no flow control (so Ctrl+S is a keystroke and cannot freeze the terminal),
	// no break or parity handling, no 8th-bit stripping (UTF-8 needs all eight).
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK |
		syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON

	// Output: post-processing off. With OPOST set the terminal expands \n to
	// \r\n, which shifts every column the painter positioned by hand.
	raw.Oflag &^= syscall.OPOST

	// Local: no echo, no line buffering, no signal generation, no extended
	// processing (so Ctrl+V and Ctrl+O reach the application).
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON |
		syscall.ISIG | syscall.IEXTEN

	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8

	// One byte is enough to return, and no inter-byte timer. The timer is
	// tempting for escape-sequence disambiguation, but doing it here would
	// block the whole read; the decoder handles that with its own deadline.
	raw.Cc[syscall.VMIN] = 1
	raw.Cc[syscall.VTIME] = 0

	if err := setState(fd, &raw); err != nil {
		return nil, err
	}
	return prev, nil
}

// size reports the terminal's dimensions in cells.
func size(fd uintptr) (int, int, error) {
	var ws winsize
	if err := ioctl(fd, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, fmt.Errorf("term: reading window size: %w", err)
	}
	return int(ws.Col), int(ws.Row), nil
}
