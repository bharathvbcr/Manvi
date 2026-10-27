// Package filelock is the cross-process exclusive file lock shared by the stores
// that several Manvi processes write at once: the spend ledger and the workflow
// catalog. The kernel drops the lock when its holder exits, so a crashed process
// never wedges the others.
package filelock

import "errors"

// ErrUnsupported is what Lock returns on a platform with no cross-process file
// lock this package knows.
var ErrUnsupported = errors.New("no cross-process file lock on this platform")
