// Package gussetcheck is Manvi's caller for the shared Gusset engine.
//
// The engine archive is DevCouncil's umbrella (one staticlib in this
// process, R14). Manvi does not link a second one. A panic from the
// diagnostic engine is reported as a failure, not treated as a match.
//
// Release binaries link the archive (scripts/release-build.sh) for `manvi
// gusset-check`; a CGO_ENABLED=0 build has no engine and returns
// ErrNotLinked. No policy gate asks the engine: fnmatch is the one
// production matcher (DevCouncil's docs/gusset-candidates.md).
package gussetcheck

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotLinked is the result of a build that does not contain the engine.
var ErrNotLinked = errors.New("gusset: engine is not linked into this build")

// SelfTest checks the linked engine (the pool ceiling is read from Gusset, so
// a build that imported a different module fails here instead of at a later
// call), then induces a deliberate panic inside the engine archive on a
// throwaway handle, proving the panic firewall (I2) against the archive this
// binary links. Rust prints the induced panic to stderr. For `manvi
// gusset-check`, never for a server.
func SelfTest(ctx context.Context) error {
	return selfTest(ctx)
}

// Shutdown releases the engine at process exit within drain: it cancels
// every job and refuses new work process-wide, then joins. An engine still
// running at the deadline is reported and left to process exit rather than
// joined, so a stuck engine cannot hang shutdown.
func Shutdown(drain time.Duration) error {
	return shutdownEngine(drain)
}

// DrainLogs writes whatever the engine's Rust side logged to w. The ring is
// bounded and evicts its oldest line, so a process that never drains it
// loses worker respawns and caught panics. A cgo-off build has nothing.
func DrainLogs(w io.Writer) (int, error) {
	return drainLogs(w)
}
