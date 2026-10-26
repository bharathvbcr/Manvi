//go:build !unix && !windows

package syncdir

import "os"

// supported is false: this platform has no directory flush this package knows,
// the same choice llm/budget made for its ledger on platforms without a Unix or
// Windows lock.
const supported = false

// flush is a no-op, see supported.
func flush(*os.File) error { return nil }
