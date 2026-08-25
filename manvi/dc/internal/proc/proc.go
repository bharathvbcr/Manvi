// Package proc holds the one bound every subprocess boundary in dc needs.
//
// The harness reaches three separate binaries — the store, the verifier, the
// repo index — through os/exec, and each of them advertises a timeout. Those
// timeouts were not what they appeared to be, and the reason is subtle enough
// that it was fixed in one boundary and left standing in the other two, which
// is why the helper lives here now rather than beside any single caller.
package proc

import "context"

// RunBounded runs one subprocess invocation under the caller's deadline and
// reports whether the deadline won.
//
// It exists because exec's own bounds do not cover the whole call.
// exec.CommandContext arms its killer only once the child exists, and
// Cmd.WaitDelay bounds Wait — both start counting after Start has returned. A
// Start that never returns is outside every one of them, so `cmd.Run()` called
// directly is an unbounded wait wearing a timeout's clothes.
//
// That gap is reachable rather than theoretical: under the race detector, with
// two dozen forks in flight on a loaded machine, Start wedged for a full
// 900-second package timeout while its 30-second context sat unused.
//
// When the deadline wins, the goroutine is abandoned. There is nothing to
// cancel — it is blocked in the kernel — and the caller must not read anything
// that goroutine still owns, because a buffer being written by an abandoned
// writer is a data race, and a partial answer read out of one would be worse
// than the timeout it replaced.
func RunBounded(ctx context.Context, run func() error) (err error, timedOut bool) {
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err = <-done:
		return err, false
	case <-ctx.Done():
		return nil, true
	}
}
