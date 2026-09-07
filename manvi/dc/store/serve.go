package store

// The served transport: the same process boundary, without a fork per call.
//
// Every store call used to be its own `dcstore` process. Measured from this
// package against the release binary on an idle machine, that was ~2.1ms of a
// ~3.9ms `diagnose` — more than half of every lease check on the write gate
// spent starting a process rather than answering a question, on the path whose
// own comment in store.go notes that "every Diagnose on the write gate and
// every Acquire comes through here". Pooled and warm, the same call measures
// ~0.03ms; both figures move with machine load, and the ratio narrows under it.
//
// What this does *not* change is the boundary. The store is still another
// process, reached over stdio, built by another toolchain: CGO_ENABLED=0, the
// single static binary, cross-compilation and crash isolation are all
// properties of the boundary being a process, and all of them survive. The
// alternatives that remove the process — cgo bindings, a shared library loaded
// at runtime, or the store compiled to wasm — each give up at least one, and
// wasm gives up the lease guarantee itself: SQLite under WASI has no POSIX file
// locking and no WAL, and dc-store refuses to open a database it cannot put in
// WAL mode.
//
// So the process stays and the *fork* goes. A child is started once and
// answers many requests over its stdin and stdout, holding one SQLite
// connection open across them — which removes the connection setup and the
// schema verification from the per-call cost as well.
//
// Two properties are load-bearing:
//
//   - **Children are pooled, not shared.** A single serialised child would
//     still be correct, but it would quietly convert concurrent acquires
//     within one harness into a queue, and the mutual exclusion this store
//     exists to provide is tested by racing them for real
//     (nAcquireElectsExactlyOneHolder). Several children keep that a race
//     between SQLite writers, which is what the partial unique index is there
//     to settle.
//   - **A child that misses a deadline is destroyed, never reused.** The reply
//     to a timed-out request may still be in flight, and a connection whose
//     stream position is unknown would answer the *next* caller with the
//     previous caller's document. Nothing about that would look like an error.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/internal/proc"
)

const (
	// serveMaxChildren bounds how many persistent store processes one client
	// keeps alive. It is a concurrency width, not a queue depth: callers past
	// it wait for a child rather than starting another.
	serveMaxChildren = 8

	// serveHeaderLimit bounds a status or length header. Headers are a word or
	// a short decimal number; a peer sending more than this is one that will
	// never send the newline being waited for.
	serveHeaderLimit = 32

	// serveStderrLimit bounds what is retained from a child's stderr. The
	// stream is drained regardless of this cap — a child that blocks writing
	// to a full stderr pipe never answers, which is the failure the MCP client
	// records at its own boundary — but only this much is kept to explain a
	// failure with.
	serveStderrLimit = 64 << 10

	// serveWaitDelay bounds how long descendants may keep holding the stdio
	// pipes after the child itself is gone, before Wait stops waiting on them.
	serveWaitDelay = 3 * time.Second
)

// errStoreFatal marks a served reply the child reported as a failure.
//
// It stands in for the exit code the one-shot path carries. decodeReply
// classifies a reply partly by whether the process failed, and four commands
// treat `ok:false` as a real answer rather than a fault — Renew reads it as
// "the lease had already expired". Without this, a served store that broke
// while renewing would be reported to the caller as an expired lease, and the
// turn would carry on having been told something false about its own state.
var errStoreFatal = errors.New("the store reported a failure")

// errServeUnsupported reports that the binary does not understand `serve`.
//
// It is not a store failure: a binary predating the served transport refuses
// the command from argv and exits before reading a single byte of the request,
// so nothing was applied and the call is safe to make again over the one-shot
// path.
var errServeUnsupported = errors.New("the store binary does not support serve")

// servedChild is one persistent dcstore process.
type servedChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	mu     sync.Mutex
	stderr cappedBuffer

	closeOnce sync.Once
}

// serveResult is one exchange's outcome, in the shape decodeReply consumes.
type serveResult struct {
	body     []byte
	overflow bool
	fatal    bool
	// stderr is what the child has said about itself over this session. It is
	// carried because decodeReply reaches for it in the one case that is
	// genuinely broken — a reply that will not parse at all — and a served
	// child's explanation is otherwise discarded with the connection.
	stderr    []byte
	transport error
}

// startChild spawns one store process in serve mode.
// The context is the pool's lifetime, never a caller's. A child outlives any
// single call, so binding it to the context of the request that happened to
// start it would kill the pooled process the moment that request returned. The
// per-call bound is applied in runServed instead, around the exchange rather
// than around the process.
func startChild(lifetime context.Context, binary, db string) (*servedChild, error) {
	// #nosec G204 -- the store binary this harness was configured with and the
	// database path it was configured with; neither reaches here from a model.
	cmd := exec.CommandContext(lifetime, binary, "--db", db, "serve")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("store: creating stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("store: creating stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("store: creating stderr pipe: %w", err)
	}
	proc.ConfigureOwnGroup(cmd)
	// Killing the child is not enough to unblock Wait. Go waits for the stdio
	// copiers to finish, so a grandchild holding the write end keeps Wait
	// pending for as long as it lives. WaitDelay bounds that second wait.
	cmd.WaitDelay = serveWaitDelay
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()
		return nil, fmt.Errorf("store: starting %s: %w", binary, err)
	}

	child := &servedChild{
		cmd:    cmd,
		stdin:  stdin,
		stdout: bufio.NewReaderSize(stdout, 64<<10),
	}
	child.stderr.limit = serveStderrLimit
	go child.drainStderr(stderr)
	return child, nil
}

// drainStderr keeps the child's stderr pipe empty and keeps a bounded prefix
// of what it said, so a failure the store already explained arrives carrying
// the explanation.
func (c *servedChild) drainStderr(r io.Reader) {
	buf := make([]byte, 4<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.mu.Lock()
			_, _ = c.stderr.Write(buf[:n])
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// diagnostics returns what the child said about itself, trimmed.
func (c *servedChild) diagnostics() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.TrimSpace(c.stderr.buf.String())
}

// close ends the child. Closing stdin is the designed shutdown: the serve loop
// reads EOF between requests and exits zero. The kill is the backstop for a
// child that is wedged somewhere it cannot see that.
func (c *servedChild) close() {
	c.closeOnce.Do(func() {
		// Closing stdin is the designed shutdown; the group kill is the
		// backstop for a child wedged somewhere it cannot see the EOF, and it
		// reaches anything the child itself spawned.
		_ = c.stdin.Close()
		if c.cmd.Process != nil {
			proc.KillGroup(c.cmd.Process.Pid)
			_ = c.cmd.Process.Kill()
		}
		_ = c.cmd.Wait()
	})
}

// writeRequest frames one argument vector.
//
// Lengths rather than delimiters: a task's appended scope is JSON carried
// through this boundary raw, and it may legally contain newlines. A wire that
// split on them would cut such a request in half and run its front half as a
// command.
func writeRequest(w io.Writer, args []string) error {
	var frame bytes.Buffer
	frame.WriteString(strconv.Itoa(len(args)))
	frame.WriteByte('\n')
	for _, arg := range args {
		frame.WriteString(strconv.Itoa(len(arg)))
		frame.WriteByte('\n')
		frame.WriteString(arg)
	}
	_, err := w.Write(frame.Bytes())
	return err
}

// readHeader reads one bounded, newline-terminated header.
func readHeader(r *bufio.Reader) (string, error) {
	var buf []byte
	for {
		b, err := r.ReadByte()
		if err != nil {
			return "", err
		}
		if b == '\n' {
			return string(buf), nil
		}
		buf = append(buf, b)
		if len(buf) > serveHeaderLimit {
			return "", fmt.Errorf("header exceeded %d bytes without a newline", serveHeaderLimit)
		}
	}
}

// exchange sends one request and reads one reply.
//
// It attempts the read even when the write failed, because the one write
// failure that is not a fault is a binary predating serve: it refuses the
// command from argv, prints its refusal, and exits without reading, so the
// write lands on a closed pipe and the explanation is already waiting on
// stdout.
func (c *servedChild) exchange(args []string) serveResult {
	writeErr := writeRequest(c.stdin, args)

	// A binary that predates serve answers argv rather than the protocol: it
	// refuses the command, prints the refusal as a JSON object, and exits
	// without ever reading. That is recognised here by its first byte, before
	// any header bound could apply to it — checking the parsed header instead
	// missed it, because a refusal is longer than a header is allowed to be and
	// so failed as an oversized header rather than as the fallback signal it
	// is. Getting that wrong reports a working store as a broken one.
	first, err := c.stdout.Peek(1)
	if err != nil {
		if writeErr != nil {
			return serveResult{transport: fmt.Errorf("store: writing request: %w", writeErr)}
		}
		return serveResult{transport: c.streamError("reading the reply", err)}
	}
	if first[0] == '{' {
		return serveResult{transport: errServeUnsupported}
	}

	status, err := readHeader(c.stdout)
	if err != nil {
		return serveResult{transport: c.streamError("reading the reply status", err)}
	}
	switch status {
	case "ok", "err":
	default:
		return serveResult{transport: fmt.Errorf(
			"store: the served reply opened with %q, which is not a status", status)}
	}

	lengthHeader, err := readHeader(c.stdout)
	if err != nil {
		return serveResult{transport: c.streamError("reading the reply length", err)}
	}
	length, err := strconv.Atoi(strings.TrimSpace(lengthHeader))
	if err != nil || length < 0 {
		return serveResult{transport: fmt.Errorf(
			"store: the served reply declared a length of %q, which is not a byte count", lengthHeader)}
	}
	if length > maxOutput {
		// Reported the way an over-cap one-shot reply is reported, and the
		// child is not read past: the stream position is now unknown, so this
		// connection is retired by the caller rather than reused.
		return serveResult{overflow: true, transport: nil}
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(c.stdout, body); err != nil {
		return serveResult{transport: c.streamError("reading the reply body", err)}
	}
	return serveResult{body: body, fatal: status == "err", stderr: []byte(c.diagnostics())}
}

// streamError explains a broken stream with whatever the child said about it.
func (c *servedChild) streamError(what string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		err = errors.New("the store process ended")
	}
	if diag := c.diagnostics(); diag != "" {
		return fmt.Errorf("store: %s: %w — the store reported: %s", what, err, diag)
	}
	return fmt.Errorf("store: %s: %w", what, err)
}

// servePool hands out persistent children, at most serveMaxChildren at once.
//
// The semaphore bounds how many callers may be in a store call at the same
// time, and therefore how many store processes exist; the idle list holds the
// children between calls. They are separate because a child is *not* held for
// the whole time its slot is: one that misses a deadline is destroyed and its
// slot freed for a fresh process.
//
// The idle list is LIFO. A caller that returns a child and immediately asks for
// another gets the same one back, so a harness making store calls one at a time
// keeps reusing a single warm process. Handing out the least recently used
// child instead filled the pool with eight processes to serve a caller that
// never had two calls in flight — visible as a p99 of one whole process spawn
// on an otherwise sub-millisecond call.
type servePool struct {
	once sync.Once
	sem  chan struct{}

	// lifetime bounds every child this pool starts, and cancelling it is what
	// makes Close reach a child that is mid-call. Without it, Close could only
	// end the idle ones and a wedged child would outlive the client that
	// started it.
	lifetime context.Context
	stop     context.CancelFunc

	mu   sync.Mutex
	idle []*servedChild
}

// init prepares the pool, deriving its lifetime from the first caller.
//
// WithoutCancel is the point: the children outlive the request that happened to
// start them, so the pool must not inherit that request's cancellation — but it
// should still inherit its values, which is what distinguishes this from
// rooting a fresh context tree beside the one the program is already using.
func (p *servePool) init(ctx context.Context) {
	p.once.Do(func() {
		lifetime, stop := context.WithCancel(context.WithoutCancel(ctx))
		// Written under the mutex because closeAll reads them without having
		// gone through init: a client closed before it was ever called must not
		// race the first call that is initialising it. Every other reader gets
		// here through init, and sync.Once orders those already.
		p.mu.Lock()
		p.sem = make(chan struct{}, serveMaxChildren)
		p.lifetime, p.stop = lifetime, stop
		p.mu.Unlock()
	})
}

// take borrows a slot, reusing an idle child or starting one for it.
func (p *servePool) take(ctx context.Context, binary, db string) (*servedChild, error) {
	p.init(ctx)
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, fmt.Errorf("store: waiting for a store process: %w", ctx.Err())
	}

	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		child := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return child, nil
	}
	p.mu.Unlock()

	// Started on its own goroutine so the caller's deadline covers the fork.
	// exec.Cmd.Start can block in the kernel, and everything that would
	// otherwise bound this child — its lifetime context, WaitDelay, the select
	// in runServed — only begins counting once Start has returned. This is the
	// same hole RunBounded closes for the one-shot path, and it is the reason
	// that path notes a wedged fork would stall the turn rather than fail it.
	//
	// nolint:contextcheck below is the design, not an oversight: the child must
	// not inherit the cancellation of whichever call happened to start it, or
	// the pooled process would die the moment that call returned. p.lifetime is
	// derived from the first caller with WithoutCancel, so values propagate and
	// cancellation does not.
	type startResult struct {
		child *servedChild
		err   error
	}
	started := make(chan startResult, 1)
	go func() {
		child, err := startChild(p.lifetime, binary, db) //nolint:contextcheck // the pool's lifetime is deliberately not the caller's
		started <- startResult{child, err}
	}()

	select {
	case result := <-started:
		if result.err != nil {
			// Release the slot: failing to start is not a reason to shrink the
			// pool permanently.
			<-p.sem
			return nil, result.err
		}
		return result.child, nil
	case <-ctx.Done():
		<-p.sem
		// The start may still complete with nobody waiting for it. That child
		// is ended here rather than left to the pool's close, because an
		// abandoned store process would otherwise hold a SQLite connection for
		// the life of the harness.
		go func() {
			if result := <-started; result.err == nil {
				result.child.close()
			}
		}()
		return nil, fmt.Errorf("store: starting a store process: %w", ctx.Err())
	}
}

// put returns a healthy child and releases its slot.
func (p *servePool) put(child *servedChild) {
	p.mu.Lock()
	p.idle = append(p.idle, child)
	p.mu.Unlock()
	<-p.sem
}

// retire destroys a child and releases its slot for a fresh one.
func (p *servePool) retire(child *servedChild) {
	if child != nil {
		child.close()
	}
	<-p.sem
}

// closeAll ends every child and makes the pool terminal.
//
// Cancelling the lifetime reaches a child that is mid-call, which closing the
// idle list alone cannot. It also means no further children start: a client
// that has been closed is closed, and a call made after it fails rather than
// quietly starting a process the caller believed it had released.
func (p *servePool) closeAll() {
	p.mu.Lock()
	idle, stop := p.idle, p.stop
	p.idle = nil
	p.mu.Unlock()

	// A pool nothing ever borrowed from has no lifetime to cancel and no
	// children to end, so closing an unused client is a no-op rather than the
	// place a fresh context tree gets rooted.
	if stop != nil {
		stop()
	}
	for _, child := range idle {
		child.close()
	}
}

// runServed performs one call over a pooled persistent child.
//
// The exchange runs on its own goroutine so the caller's deadline bounds it,
// matching what RunBounded does for the one-shot path: a write to a child that
// is not reading, or a read from one that is not answering, is otherwise
// outside every bound this client advertises.
func (c *Client) runServed(ctx context.Context, command string, args []string) (serveResult, error) {
	child, err := c.pool.take(ctx, c.Binary, c.DB)
	if err != nil {
		return serveResult{}, err
	}

	done := make(chan serveResult, 1)
	go func() { done <- child.exchange(args) }()

	select {
	case result := <-done:
		switch {
		case errors.Is(result.transport, errServeUnsupported):
			// Not a fault, and not this child's fault either: retire it so the
			// next caller does not pay for the same discovery.
			c.pool.retire(child)
			return serveResult{}, errServeUnsupported
		case result.transport != nil:
			// The stream is broken. This is reported as the transport failure
			// it is, rather than handed on as a reply: an empty body decoded as
			// one would reach the caller as "the store returned unparseable
			// output", naming the store for a fault that was the connection's.
			c.pool.retire(child)
			return serveResult{}, result.transport
		case result.overflow:
			// A reportable outcome — the caller is told the reply was over the
			// cap — but the document was left unread, so this connection's
			// stream position is no longer known and it cannot be reused.
			c.pool.retire(child)
			return result, nil
		default:
			c.pool.put(child)
			return result, nil
		}
	case <-ctx.Done():
		// The reply may still arrive. A child whose stream holds an unread
		// document would answer the next caller with this caller's reply, so
		// it is destroyed rather than returned.
		c.pool.retire(child)
		return serveResult{}, fmt.Errorf("store: %s did not return within %s: %w",
			command, c.timeout(), ctx.Err())
	}
}

// Close ends the persistent store processes this client is holding.
//
// It is not required for correctness — closing this process's ends of the
// pipes lets each child read EOF and exit on its own — but a caller that is
// done with a client should not have to exit to release them.
func (c *Client) Close() { c.pool.closeAll() }
