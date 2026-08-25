// Package proc owns the subprocess wait bound shared by every MANVI process
// boundary.
package proc

import "context"

// RunBounded runs one subprocess invocation under the caller's deadline and
// reports whether the deadline won.
//
// exec.CommandContext arms its killer only after the child exists, and
// Cmd.WaitDelay also starts after Start returns. A Start blocked in the kernel
// is therefore outside both bounds. Running the complete call here makes the
// caller's context bound cover Start as well as Wait.
//
// When the deadline wins, the goroutine is abandoned. The caller must not read
// state the goroutine may still be writing, such as an output buffer.
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
