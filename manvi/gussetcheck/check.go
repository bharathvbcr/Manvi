// Package gussetcheck is Manvi's caller for the shared Gusset engine.
//
// The engine archive is DevCouncil's umbrella (one staticlib in this
// process, R14). Manvi does not link a second one. A panic from the
// diagnostic engine is reported as a failure, not treated as a match.
//
// Release binaries link the archive (scripts/release-build.sh), and every
// policy gate they build asks the engine through Matcher. A CGO_ENABLED=0
// build has no engine: it returns ErrNotLinked and its gates use fnmatch.
// A cgo build keeps a poisoned handle failed; a transient failure is retried.
package gussetcheck

import (
	"context"
	"errors"
	"io"
	"sync"
	"syscall"
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

// ReadyRetry is how long a transient Ready failure is served from cache
// before the check runs again.
const ReadyRetry = 30 * time.Second

var (
	readyMu sync.Mutex
	// readyOK is set once the check has passed; it is never cleared.
	readyOK bool
	// readyLatched is a failure that is a verdict, not an outage: no engine
	// in this build, a poisoned or panicking engine, or a parity mismatch.
	readyLatched error
	// readyLast is the last transient failure and when it happened.
	readyLast error
	readyAt   time.Time
	// runReady and readyRetry are seams for tests.
	runReady   = Run
	readyRetry = ReadyRetry
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

// Ready reports whether the engine has passed its check. A pass is kept for
// the life of the process. A failure that is a verdict stays failed: a
// poisoned handle does not start answering policy checks on the next call,
// and ErrNotLinked is the whole answer for a cgo-off build.
//
// A transient failure is not a verdict. This was a sync.Once, so a first
// check that ran out of its 5 s budget on a loaded host, or met EMFILE
// opening the handle, refused every later policy check until the process
// restarted — while gussetfn itself deliberately retries a failed Open. Such
// a failure is served from cache for ReadyRetry, then the check runs again.
func Ready() error {
	readyMu.Lock()
	defer readyMu.Unlock()
	switch {
	case readyOK:
		return nil
	case readyLatched != nil:
		return readyLatched
	case readyLast != nil && time.Since(readyAt) < readyRetry:
		return readyLast
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := runReady(ctx)
	switch {
	case err == nil:
		readyOK, readyLast = true, nil
	case transient(err):
		readyLast, readyAt = err, time.Now()
	default:
		readyLatched = err
	}
	return err
}

// transient reports a failure that says nothing about the engine: the
// check's own deadline, or the OS refusing a resource (a pipe, a thread).
func transient(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, context.Canceled) ||
		errors.As(err, &errno)
}

// DrainLogs writes whatever the engine's Rust side logged to w. The ring is
// bounded and evicts its oldest line, so a process that never drains it
// loses worker respawns and caught panics. A cgo-off build has nothing.
func DrainLogs(w io.Writer) (int, error) {
	return drainLogs(w)
}
