// Package gussetcheck is Manvi's caller for the shared Gusset engine.
//
// The engine archive is DevCouncil's umbrella (one staticlib in this
// process, R14). Manvi does not link a second one. A panic from the
// diagnostic engine is reported as a failure, not treated as a match.
//
// The release binary is built with CGO_ENABLED=0, so it does not link
// the archive. That build returns ErrNotLinked. A cgo build calls the
// engine and keeps a poisoned handle failed.
package gussetcheck

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrNotLinked is the result of a build that does not contain the engine.
// Policy checks do not treat it as a dead handle: there is no handle.
var ErrNotLinked = errors.New("gusset: engine is not linked into this build")

// Run checks the linked engine. The pool ceiling is read from Gusset so a
// build that imported a different module fails here instead of at a later call.
func Run(ctx context.Context) error {
	return check(ctx)
}

var (
	readyOnce sync.Once
	readyErr  error
)

// SelfTest is Run plus a deliberate panic inside the engine archive on a
// throwaway handle, proving the panic firewall (I2) against the archive this
// binary links. Rust prints the induced panic to stderr. For `manvi
// gusset-check`, never for a server: Ready runs Run, which panics nothing.
func SelfTest(ctx context.Context) error {
	return selfTest(ctx)
}

// Close releases the engine's shared handle. It joins the engine's workers
// without a bound; at process exit use Shutdown. A cgo-off build has nothing
// to release.
func Close() error {
	return closeEngine()
}

// Shutdown releases the engine at process exit within drain: it cancels
// every job and refuses new work process-wide, then joins. An engine still
// running at the deadline is reported and left to process exit rather than
// joined, so a stuck engine cannot hang shutdown.
func Shutdown(drain time.Duration) error {
	return shutdownEngine(drain)
}

// Ready runs the engine check once per process. A failure stays failed:
// a poisoned handle does not start answering policy checks on the next call.
// ErrNotLinked also stays, which is the whole answer for a cgo-off build.
func Ready() error {
	readyOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		readyErr = Run(ctx)
	})
	return readyErr
}
