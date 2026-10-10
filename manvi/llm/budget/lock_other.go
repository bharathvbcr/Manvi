//go:build !unix && !windows

package budget

import (
	"context"
	"errors"
)

// lockFile refuses on platforms with no cross-process lock this package knows.
// A cap that cannot be shared safely must stop the provider from starting, not
// let concurrent sessions overspend it.
func lockFile(context.Context, string) (func(), error) {
	return nil, errors.New("budget: no cross-process file lock on this platform; the spend cap cannot be enforced")
}

func syncDir(string) error { return nil }

// processGone never claims a process is gone where it cannot check.
func processGone(int) bool { return false }
