//go:build linux

package term

import (
	"fmt"
	"os"
	"unsafe"
)

const (
	tiocsptlck = 0x40045431
	tiocgptn   = 0x80045430
	tiocswinsz = 0x5414
)

func slaveName(master *os.File) (string, error) {
	unlock := int32(0)
	if err := ioctlPtr(master.Fd(), tiocsptlck, unsafe.Pointer(&unlock)); err != nil {
		return "", fmt.Errorf("unlocking the pty: %w", err)
	}
	var n uint32
	if err := ioctlPtr(master.Fd(), tiocgptn, unsafe.Pointer(&n)); err != nil {
		return "", fmt.Errorf("reading the pty number: %w", err)
	}
	return fmt.Sprintf("/dev/pts/%d", n), nil
}
