package devcouncil

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
)

// procWaitDelay bounds how long a descendant may hold the stdio pipes after
// the direct child is gone. Without it, cancelling the context kills the child
// but os/exec's copy goroutine keeps waiting on a pipe a grandchild — a git
// hook, a credential helper, a command the agent backgrounded — still holds
// open, so a bounded context still produces an unbounded wait.
const procWaitDelay = 5 * time.Second

// procSpec is one child process this package runs.
type procSpec struct {
	argv []string
	dir  string
	// env is the child's whole environment; nil inherits this process's.
	env   []string
	stdin io.Reader
	// timeout bounds the call on top of whatever deadline ctx carries. Zero
	// leaves the bound to ctx, which must then have a deadline.
	timeout time.Duration
	// stdoutLimit and stderrLimit cap each capture; zero keeps none of that
	// stream. combined sends stderr into the stdout capture, under
	// stdoutLimit, in the order the child wrote it.
	stdoutLimit, stderrLimit int
	combined                 bool
}

// procResult is what a bounded call produced.
type procResult struct {
	stdout, stderr []byte
	// stdoutNote and stderrNote say what each cap discarded, empty when the
	// capture is whole. A capped capture is never a complete one.
	stdoutNote, stderrNote string
	// stdoutDropped is how many bytes of stdout the cap discarded, so a caller
	// can know the whole size of what the child wrote without keeping all of
	// it. Zero exactly when stdoutNote is empty.
	stdoutDropped int
	// exitCode is the child's own exit status; -1 when a signal ended it.
	exitCode int
	// err is nil on a clean exit, an *exec.ExitError on a non-zero one, and
	// otherwise the reason the child could not be run or was cancelled.
	err error
	// timedOut reports that the deadline ended the call. err is nil then: a
	// timeout is its own outcome, not a failure to run.
	timedOut bool
}

// failedToRun reports an error that is not the child's own exit status.
func (r procResult) failedToRun() bool {
	var exitErr *exec.ExitError
	return r.err != nil && !errors.As(r.err, &exitErr)
}

// runProc is the one place this package starts a child process.
//
// Every property below was learned at one call site and missing at the next,
// which is the argument for a single owner rather than five runners that each
// re-derive them:
//
//   - The child leads its own process group, and the group is killed when the
//     deadline fires and again on every other way out, so nothing the child
//     started survives the call — `sleep 30 &` used to outlive the tool call
//     that ran it, unattached to anything that would clean it up.
//   - proc.RunBounded runs the whole call, Start included, under the deadline.
//   - Both streams are captured through limitWriter, which caps without
//     killing the child and counts what it dropped.
//   - procWaitDelay unblocks Wait when a descendant holds a pipe.
//
// The captures are read under a lock. When the deadline wins, RunBounded
// abandons the goroutine still copying output, and what was captured before
// the deadline is still the evidence the caller reports.
func runProc(ctx context.Context, spec procSpec) procResult {
	if len(spec.argv) == 0 {
		return procResult{err: errors.New("no command to run")}
	}
	if spec.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.timeout)
		defer cancel()
	} else if _, ok := ctx.Deadline(); !ok {
		return procResult{err: errors.New("refusing to run " + spec.argv[0] + " without a time bound")}
	}

	// #nosec G204 -- argv is built by this package's callers from fixed verbs
	// and gate-approved arguments, never through a shell unless the caller is
	// the shell tool itself.
	cmd := exec.CommandContext(ctx, spec.argv[0], spec.argv[1:]...)
	proc.ConfigureGroup(cmd)
	cmd.WaitDelay = procWaitDelay
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	cmd.Stdin = spec.stdin

	stdout := newCapture(spec.stdoutLimit)
	stderr := stdout
	if !spec.combined {
		stderr = newCapture(spec.stderrLimit)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	var exitCode int
	runErr, timedOut := proc.RunBounded(ctx, func() error {
		err := cmd.Run()
		// Reaped on a clean or failed exit as well as on the deadline. Read
		// here, inside the goroutine that owns cmd, because on a timeout this
		// goroutine may still be writing it.
		if cmd.Process != nil {
			proc.KillGroup(cmd.Process.Pid)
		}
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return err
	})

	res := procResult{}
	res.stdout, res.stdoutNote, res.stdoutDropped = stdout.snapshot()
	if !spec.combined {
		res.stderr, res.stderrNote, _ = stderr.snapshot()
	}
	if timedOut {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			res.timedOut = true
		} else {
			res.err = ctx.Err()
		}
		return res
	}
	res.exitCode = exitCode
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// os/exec returns this only when the child itself exited cleanly and
		// a leftover descendant kept the pipes open past the grace period.
		// The command succeeded; plumbing cleanup is not its failure.
		runErr = nil
	}
	res.err = runErr
	return res
}

// lockedCapture serialises a limitWriter between the copy goroutine and a
// reader that may arrive while it is still running.
type lockedCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
	lw  limitWriter
}

func newCapture(limit int) *lockedCapture {
	c := &lockedCapture{}
	c.lw = limitWriter{w: &c.buf, limit: limit}
	return c
}

func (c *lockedCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lw.Write(p)
}

// snapshot copies what has been captured so far, the truncation note, and how
// many bytes the cap discarded.
func (c *lockedCapture) snapshot() ([]byte, string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.buf.Bytes()), c.lw.truncationNote(), c.lw.dropped
}

// limitWriter forwards at most limit bytes to w and silently discards the rest,
// while remaining a well-behaved io.Writer.
//
// That second half is the whole point. io.Writer requires a short return to
// carry a non-nil error, and os/exec copies a child's stdout with io.Copy,
// which enforces it: the write that straddled the cap used to return
// (truncated, nil), io.Copy turned that into io.ErrShortWrite, os/exec closed
// the pipe, and the child took SIGPIPE. A command that had already run to
// completion was then reported as a failure — "failed to execute command" from
// exec_command, and "git … failed with exit code 141" from the git tools — for
// work whose side effects were already on disk. Capping output is not a reason
// to kill the process producing it, and it is never a reason to call a
// successful command failed.
//
// dropped counts what the cap ate, so a truncated capture can say so rather
// than passing a partial sample off as the whole output.
type limitWriter struct {
	w       io.Writer
	limit   int
	wrote   int
	dropped int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if l.wrote >= l.limit {
		l.dropped += len(p)
		return len(p), nil
	}
	remaining := l.limit - l.wrote
	if len(p) <= remaining {
		n, err := l.w.Write(p)
		l.wrote += n
		return n, err
	}
	n, err := l.w.Write(p[:remaining])
	l.wrote += n
	if err != nil {
		// A real downstream failure is still short, and still an error.
		return n, err
	}
	l.dropped += len(p) - remaining
	return len(p), nil
}

// truncated reports whether the cap discarded anything.
func (l *limitWriter) truncated() bool { return l.dropped > 0 }

// truncationNote renders the discarded byte count for a result, so a capped
// capture is never presented as complete coverage.
func (l *limitWriter) truncationNote() string {
	if !l.truncated() {
		return ""
	}
	return truncationNoteFor(l.wrote, l.dropped)
}

// truncationNoteFor is the wording of a capped capture, given what was kept
// and what was dropped.
//
// Separate from truncationNote so a caller that captured more than its share
// and cut the rest itself — the untracked-file fan-out in verify.go does —
// reports the cut in exactly the words the cap would have used. Two spellings
// of one fact would be a diff whose degradation notes depend on how it was
// scheduled.
func truncationNoteFor(wrote, dropped int) string {
	return fmt.Sprintf("[output truncated: %d bytes captured, %d further bytes discarded]",
		wrote, dropped)
}
