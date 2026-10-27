//go:build !windows && !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !illumos

package filelock

import (
	"context"
	"os"
)

// Lock refuses on platforms with no cross-process lock this package knows. A
// caller that must serialise with other processes has to stop rather than go on
// unlocked.
func Lock(context.Context, string) (func(), error) {
	return nil, ErrUnsupported
}

// LockFile refuses for the same reason as Lock, and closes f as its contract says.
func LockFile(_ context.Context, f *os.File) (func(), error) {
	_ = f.Close()
	return nil, ErrUnsupported
}
