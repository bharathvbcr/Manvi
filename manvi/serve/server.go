package serve

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"sync"
)

// maxLineBytes bounds one request line.
//
// bufio.Scanner would cap this anyway, at 64 KiB, and would then report the
// overflow as end-of-input — which a host reads as "the sidecar exited". The
// reader below is explicit instead: an oversized line is refused with an
// error that carries the request's id where one can be recovered, and the
// session continues — matching the rule that errors are values, not stream
// terminations.
const maxLineBytes = 8 << 20

// idHeadBytes is how much of an oversized line is kept for best-effort id
// recovery. The id of a well-formed request is in its first object fields, so
// a few kilobytes covers every host that marshals id first — and a hostile
// line costs at most this much memory rather than the whole 8 MiB.
const idHeadBytes = 4 << 10

// Server serves the harness's planes over one stdio pair.
type Server struct {
	// hardRules and allowNeighbors mirror the policy flags. They are fixed for
	// the process lifetime, because they are operator posture rather than
	// per-request arguments — a host that could weaken hard rules per call
	// would make the gate advisory.
	hardRules      bool
	allowNeighbors bool
	posture        Posture

	// out serializes writes. Events and responses are produced by concurrent
	// handlers, and two goroutines interleaving partial lines would corrupt
	// the stream in a way neither side could resynchronise.
	mu  sync.Mutex
	out *bufio.Writer

	// chat holds per-conversation compaction ledgers and calibrators. It is
	// the only state that survives between requests, and it is what makes
	// compaction one-way rather than recomputed each step.
	chat sessionTable
}

// Options configures a Server.
type Options struct {
	// HardRules enforces the rungs that protect the repository and its
	// credentials. Defaults to true; false is honoured but reported, because a
	// gate that was turned off must never look like a gate that passed.
	HardRules bool
	// AllowNeighbors mirrors policy.scope.allow_neighbors.
	AllowNeighbors bool
	// Posture decides what a taskless denial means. Empty means PostureHost,
	// since a process being driven over stdio by another program is by
	// definition embedded.
	Posture Posture
}

// New builds a Server.
func New(w io.Writer, opts Options) *Server {
	posture := opts.Posture
	if posture == "" {
		posture = PostureHost
	}
	return &Server{
		hardRules:      opts.HardRules,
		allowNeighbors: opts.AllowNeighbors,
		posture:        posture,
		out:            bufio.NewWriter(w),
	}
}

// ops is the served set, reported by hello so a host can degrade rather than
// discover an unimplemented op mid-turn.
var ops = []string{
	OpHello,
	OpPolicyCheckFile,
	OpPolicyCheckCommand,
	OpCapabilityProbe,
	OpChatPrepare,
	OpChatSettle,
	OpChatForget,
}

// Serve reads requests until r is exhausted or ctx is cancelled.
//
// It returns nil on clean end-of-input: the host closing stdin is how a
// sidecar is asked to exit, so that is a successful shutdown rather than a
// read error. Cancellation (SIGINT/SIGTERM via NotifyContext) ends the loop
// the same way. It returns non-nil only when the stream itself failed.
//
// The blocking read cannot observe the context, so a producer goroutine feeds
// lines through a channel and the loop selects between them and Done(). The
// producer may end up parked on a read of a stdin nobody will ever close; it
// exits when cancellation fires, which is exactly the case where it matters,
// and dies with the process otherwise.
func (s *Server) Serve(ctx context.Context, r io.Reader) error {
	reader := bufio.NewReaderSize(r, 64<<10)

	type readResult struct {
		line []byte
		err  error
	}
	lines := make(chan readResult)
	go func() {
		defer close(lines)
		for {
			line, _, err := readLine(reader, maxLineBytes)
			select {
			case lines <- readResult{line, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				// An oversized line is refused downstream and the session
				// lives on, so the producer keeps reading past it. Anything
				// else — EOF, a failed stream — ends the feed.
				if _, oversize := err.(errLineTooLarge); oversize {
					continue
				}
				return
			}
		}
	}()

	for {
		var line []byte
		var readErr error
		select {
		case <-ctx.Done():
			return s.flush()
		case rr, ok := <-lines:
			if !ok {
				return s.flush()
			}
			line, readErr = rr.line, rr.err
		}
		if readErr == io.EOF {
			return s.flush()
		}
		if readErr != nil {
			if oversized, ok := readErr.(errLineTooLarge); ok {
				// An oversized request is refused like any other: one
				// response, correlated by id where the head preserved one,
				// and the session lives on. Killing the connection here would
				// take every other in-flight call down with it — the exact
				// outcome the protocol refuses to accept for a bad request —
				// and the host would read it as "the sidecar exited".
				s.writeResponse(Response{
					ID: recoverID(oversized.head),
					OK: false,
					Error: &Error{
						Code:    ErrTooLarge,
						Message: fmt.Sprintf("request line exceeds %d bytes; send fewer messages or compact first", maxLineBytes),
					},
				})
				if flushErr := s.flush(); flushErr != nil {
					return flushErr
				}
				continue
			}
			_ = s.flush()
			return readErr
		}
		if len(line) == 0 {
			continue
		}
		var req Request
		if jsonErr := json.Unmarshal(line, &req); jsonErr != nil {
			// No id to correlate against — the line did not parse — so the
			// error goes out with an empty id. A host cannot route it to a
			// waiting call, but it can log it, which beats silence.
			s.writeResponse(Response{
				OK:    false,
				Error: badRequest("malformed request line: %v", jsonErr),
			})
			continue
		}
		if req.ID == "" {
			s.writeResponse(Response{
				OK:    false,
				Error: badRequest("request is missing an id; a response the host cannot route is indistinguishable from a hang"),
			})
			continue
		}

		// Handled inline rather than per-request goroutine. Every op served
		// today is either pure computation or one bounded HTTP round-trip
		// against loopback, and a host that wants concurrency can open a
		// second sidecar. Spawning a goroutine per line would let a host with
		// a runaway loop create unbounded work here, and the bound would have
		// to be invented; there is no bound to invent while this is serial.
		s.dispatch(ctx, req)
		if err := s.flush(); err != nil {
			return err
		}
	}
}

// recovered runs one operation, converting a panic into an E_INTERNAL error.
//
// A sidecar is not a CLI. A CLI that panics has failed one command in front of
// the person who ran it; this process is spawned by another program — DevPrism
// drives it over stdio — and a panic here takes the whole integration down
// mid-session, with the host left waiting on an id whose answer will never come.
// The two failures are already treated as the same thing a few lines below,
// where an unserialisable result is reported rather than dropped precisely
// because "a host waiting on this id would otherwise hang forever". A panic is
// that same hang plus a dead process.
//
// The panic is reported, never swallowed: E_INTERNAL says the defect is in the
// harness rather than in the request, and Retryable is false because the same
// bytes panic the same way. The stack goes to stderr, where a host that
// captures it keeps something to file a bug from — returning it over the wire
// would instead hand a peer program the harness's internals.
//
// No handler served today panics; the read loop, the params decoding and the
// session table are all careful about it. This is the bound that keeps a future
// one from being an outage.
func recovered(fn func() (any, *Error)) (result any, opErr *Error) {
	defer func() {
		panicked := recover()
		if panicked == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "manvi serve: internal defect: %v\n%s\n", panicked, debug.Stack())
		result = nil
		opErr = &Error{
			Code: ErrInternal,
			Message: fmt.Sprintf(
				"the harness failed while serving this request: %v; "+
					"this is a defect in manvi, not in the request", panicked),
			Retryable: false,
		}
	}()
	return fn()
}

// dispatch runs one request and writes exactly one response.
func (s *Server) dispatch(ctx context.Context, req Request) {
	result, opErr := recovered(func() (any, *Error) { return s.handle(ctx, req) })
	if opErr != nil {
		s.writeResponse(Response{ID: req.ID, OK: false, Error: opErr})
		return
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		// The handler produced something unserialisable. That is a defect
		// here, not in the request, and it is reported as one rather than
		// dropped — a host waiting on this id would otherwise hang forever.
		s.writeResponse(Response{
			ID: req.ID, OK: false,
			Error: &Error{Code: ErrInternal, Message: fmt.Sprintf("encoding %s result: %v", req.Op, err)},
		})
		return
	}
	s.writeResponse(Response{ID: req.ID, OK: true, Result: encoded})
}

func (s *Server) handle(ctx context.Context, req Request) (any, *Error) {
	switch req.Op {
	case OpHello:
		return s.hello(req.Params)
	case OpPolicyCheckFile:
		return s.checkFile(req.Params)
	case OpPolicyCheckCommand:
		return s.checkCommand(req.Params)
	case OpCapabilityProbe:
		return s.probe(ctx, req.Params)
	case OpChatPrepare:
		return s.prepare(req.Params)
	case OpChatSettle:
		return s.settle(req.Params)
	case OpChatForget:
		return s.forget(req.Params)
	default:
		return nil, badRequest("unknown op %q (this build serves: %v)", req.Op, ops)
	}
}

func (s *Server) hello(raw json.RawMessage) (any, *Error) {
	var p HelloParams
	// Params are optional on hello: a host that only wants to learn the
	// version has nothing to declare.
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, badRequest("hello params: %v", err)
		}
	}
	if p.Protocol != 0 && p.Protocol != ProtocolVersion {
		return nil, &Error{
			Code: ErrBadRequest,
			Message: fmt.Sprintf(
				"protocol mismatch: host speaks %d, this harness speaks %d — "+
					"a field that changed meaning would be mis-decoded rather than rejected, "+
					"so the mismatch is refused here instead of surfacing as a wrong policy decision",
				p.Protocol, ProtocolVersion),
		}
	}
	return HelloResult{Protocol: ProtocolVersion, Ops: ops, Posture: string(s.posture)}, nil
}

// Emit writes a non-terminal event for an in-flight request.
func (s *Server) Emit(id, name string, data any) {
	encoded, err := json.Marshal(data)
	if err != nil {
		// An event is progress reporting; losing one is survivable and must
		// not fail the request it belongs to.
		return
	}
	s.writeLine(Event{ID: id, Event: name, Data: encoded})
}

func (s *Server) writeResponse(resp Response) { s.writeLine(resp) }

func (s *Server) writeLine(v any) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(encoded)
	_ = s.out.WriteByte('\n')
}

func (s *Server) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.Flush()
}

// errLineTooLarge reports a request line past the cap.
//
// head holds the first idHeadBytes of the offending line so the caller can
// still correlate a refusal with the request that caused it. The rest was
// never retained: keeping 8 MiB per hostile line would let a host turn one
// refused write into unbounded memory here.
type errLineTooLarge struct {
	max  int
	head []byte
}

func (e errLineTooLarge) Error() string {
	return fmt.Sprintf("request line exceeds %d bytes", e.max)
}

// recoverID pulls the correlation id out of an oversized line's head.
//
// Best effort by design: encoding/json is not used because the head is a
// fragment, not an object. A short scan for `"id":"…"` covers every host that
// marshals its request struct in field order — which ours does — and anything
// else degrades to an empty id, i.e. a visible but unroutable refusal, which
// is still strictly better than the session death it replaces.
func recoverID(head []byte) string {
	const key = `"id"`
	i := indexBytes(head, []byte(key))
	if i < 0 {
		return ""
	}
	j := i + len(key)
	// Skip whitespace and the colon.
	for j < len(head) && (head[j] == ' ' || head[j] == ':' || head[j] == '\t' || head[j] == '\n' || head[j] == '\r') {
		j++
	}
	if j >= len(head) || head[j] != '"' {
		return ""
	}
	j++
	start := j
	for j < len(head) && head[j] != '"' {
		if head[j] == '\\' {
			// An escaped byte cannot appear in the ids this protocol
			// correlates (they are caller-chosen opaque strings), but skipping
			// the pair keeps a backslash from ending the scan early.
			j++
			if j >= len(head) {
				return ""
			}
		}
		j++
	}
	if j >= len(head) {
		// The value ran past the retained head; refuse to guess.
		return ""
	}
	return string(head[start:j])
}

func indexBytes(b []byte, sub []byte) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		match := true
		for k := range sub {
			if b[i+k] != sub[k] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

// readLine reads one newline-terminated line, refusing one longer than max.
//
// It returns the line without its terminator, and io.EOF only when no bytes
// were read — a final line without a trailing newline is returned as a line,
// because a host that writes one and waits for the answer would otherwise wait
// forever. A line longer than max returns an errLineTooLarge whose head
// carries the first idHeadBytes of the line; the remainder is drained so the
// next read starts at a real line boundary rather than parsing the tail as
// requests the host never sent.
func readLine(r *bufio.Reader, max int) ([]byte, []byte, error) {
	var buf []byte
	var head []byte
	for {
		chunk, err := r.ReadSlice('\n')
		if len(buf)+len(chunk) > max {
			// Drain to the terminator so the *next* read starts at a real line
			// boundary. Without this the oversized line's tail is parsed as
			// requests, and a host gets a burst of malformed-line errors it
			// never sent anything to cause.
			if err == bufio.ErrBufferFull {
				_ = discardTo(r, '\n')
			}
			// Every earlier chunk is already mirrored into head; only this one
			// can still contribute bytes toward the retained prefix.
			if len(head) < idHeadBytes {
				take := len(chunk)
				if take > idHeadBytes-len(head) {
					take = idHeadBytes - len(head)
				}
				head = append(head, chunk[:take]...)
			}
			return nil, head, errLineTooLarge{max: max, head: head}
		}
		buf = append(buf, chunk...)
		if len(head) < idHeadBytes {
			take := len(chunk)
			if take > idHeadBytes-len(head) {
				take = idHeadBytes - len(head)
			}
			head = append(head, chunk[:take]...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				return trimNewline(buf), head, nil
			}
			return nil, head, err
		}
		return trimNewline(buf), head, nil
	}
}

func discardTo(r *bufio.Reader, delim byte) error {
	for {
		_, err := r.ReadSlice(delim)
		if err == bufio.ErrBufferFull {
			continue
		}
		return err
	}
}

func trimNewline(b []byte) []byte {
	b = trimSuffixByte(b, '\n')
	return trimSuffixByte(b, '\r')
}

func trimSuffixByte(b []byte, c byte) []byte {
	if len(b) > 0 && b[len(b)-1] == c {
		return b[:len(b)-1]
	}
	return b
}

func badRequest(format string, args ...any) *Error {
	return &Error{Code: ErrBadRequest, Message: fmt.Sprintf(format, args...)}
}
