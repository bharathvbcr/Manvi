//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package term

import "syscall"

// The BSD family, macOS included, spells the termios ioctls TIOCGETA/TIOCSETA.
const (
	ioctlGetTermios = syscall.TIOCGETA
	ioctlSetTermios = syscall.TIOCSETA
)
