//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package term

import (
	"fmt"
	"os"
	"unsafe"
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
	if err := ioctlPtr(master.Fd(), tiocptygname, unsafe.Pointer(&buf[0])); err != nil {
		return "", fmt.Errorf("reading the pty name: %w", err)
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return string(buf[:n]), nil
}
