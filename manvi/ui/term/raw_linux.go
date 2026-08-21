//go:build linux

package term

import "syscall"

// Linux spells the termios ioctls TCGETS/TCSETS.
const (
	ioctlGetTermios = syscall.TCGETS
	ioctlSetTermios = syscall.TCSETS
)
