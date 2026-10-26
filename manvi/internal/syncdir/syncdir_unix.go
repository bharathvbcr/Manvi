//go:build unix

package syncdir

import "os"

// supported is true where a directory can be flushed.
const supported = true

// flush fsyncs the directory handle. Durability is decided by Sync, which is
// returned; closing the read-only handle afterwards cannot undo or confirm
// anything.
func flush(d *os.File) error { return d.Sync() }
