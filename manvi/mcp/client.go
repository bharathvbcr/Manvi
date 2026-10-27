package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/proc"
	"github.com/bharathvbcr/Manvi/manvi/childenv"
)

// serverError is a JSON-RPC error a server answered a call with. It unwraps to
// the *RPCError, so a caller can read the code and data (-32022 carries the
// revisions the server does serve) instead of matching the message.
type serverError struct {
	server string
	rpc    *RPCError
}

func (e *serverError) Error() string {
	return fmt.Sprintf("mcp: %s returned error: %s (code %d)", e.server, e.rpc.Message, e.rpc.Code)
}

func (e *serverError) Unwrap() error { return e.rpc }

// ServerConfig configures an MCP server process.
type ServerConfig struct {
	Name    string            `json:"name,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
	// EnvPassthrough names variables to forward from this process's own
	// environment, for a declaration that needs a secret it must not carry in
	// plaintext.
	//
	// It exists so the allowlist below is usable rather than something
	// operators route around by pasting a live token into a tracked file.
	// Forwarding is opt-in, per server and per variable, and the names are
	// covered by the authorization fingerprint — so a declaration cannot start
	// forwarding a new variable without being authorized again.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`

	// Origin says where this declaration came from, and it is deliberately not
	// settable from JSON: a file in a checked-out tree must not be able to
	// claim it was registered in-process. Manager.register stamps it.
	Origin Origin `json:"-"`
	// Source names the file this declaration was read from, so a refusal can
	// tell an operator which file to go and look at. Also not settable from
	// JSON, for the same reason.
	Source string `json:"-"`
}

// Client is a JSON-RPC 2.0 client connected to an MCP server process over stdio.
type Client struct {
	cfg      ServerConfig
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser
	writerMu sync.Mutex
	// writeWedged latches once a stdin write has run past writeTimeout. See
	// writeFrame: after that the framing on stdin can no longer be accounted
	// for, so the connection is finished rather than reused.
	writeWedged atomic.Bool
	reqCounter  atomic.Int64

	pendingMu sync.Mutex
	pending   map[int64]chan *Response

	// initMu guards the three fields below and is only ever held for a few
	// instructions, never across a request: a second Initialize caller must be
	// able to wait on its own context, and it cannot while queued on a mutex.
	initMu sync.Mutex
	// initResult is set once, by the flight that completed the handshake and
	// sent notifications/initialized. It is never cleared.
	initResult *InitializeResult
	// initFatal is the error that ended the session's handshake for good. See
	// Initialize.
	initFatal error
	// initFlight is the handshake in progress, nil when none is.
	initFlight   *initFlight
	closed       atomic.Bool
	doneCh       chan struct{}
	stderrDone   chan struct{}
	serverErrors []string
	errMu        sync.Mutex
}

const (
	// maxStdoutLine bounds one JSON-RPC frame from the server. A line past it
	// is skipped with a recorded diagnostic rather than allowed to end the
	// session: the next line may be perfectly good.
	maxStdoutLine = 16 << 20
	// maxStderrLine bounds one retained stderr line. Diagnostics are worth
	// keeping only while they are small; a server emitting megabyte log lines
	// must not grow the retained buffer without bound. A line past it is
	// skipped — with a diagnostic saying so — and draining continues, because a
	// stderr reader that stops is a child that blocks in write(2).
	maxStderrLine = 1 << 20
	// defaultCallTimeout applies to calls made on a context with no deadline.
	// An unbounded wait on a wedged server freezes the whole tool surface;
	// operators who need longer set Timeout in the server config.
	defaultCallTimeout = 120 * time.Second
	// writeTimeout bounds how long a stdin write may block. A child that
	// stopped draining its stdin while its stdout stays alive would otherwise
	// hold writerMu forever and freeze every other call in the queue.
	writeTimeout = 5 * time.Second
	// waitDelay bounds how long descendants may keep holding the stdio pipes
	// once the direct child is gone.
	waitDelay = 3 * time.Second
	// stderrDrainBound is how long the stdout reader waits for the stderr
	// reader to finish after stdout ends. A server that explains its death
	// and exits writes that line and closes both pipes together, and the
	// stdout EOF can be observed first. Failing the pending call at that
	// moment reports a bare close and an empty Diagnostics, which is what
	// GitHub's macOS runner did to TestWhatTheServerSaysAboutItsFailureReachesTheCaller.
	stderrDrainBound = 2 * time.Second
)

// Caps on everything a server sends back.
//
// Every byte here is attacker-controlled: the server is a separate program,
// frequently one a checked-out repository asked for, and its listings are
// pasted straight into a model's context. Unbounded, a server could answer
// tools/list with 120,000 tools or a 15 MiB description and exhaust the harness
// before anyone read a word of it. A listing past a cap is refused whole rather
// than truncated, because a truncated listing is a capped sample that reads
// like complete coverage.
const (
	maxToolsPerServer     = 4096
	maxToolNameLen        = 256
	maxToolDescLen        = 64 << 10
	maxToolSchemaBytes    = 256 << 10
	maxResourcesPerServer = 4096
	maxResourceFieldLen   = 8 << 10
	// maxResourceReadBytes bounds one resources/read reply. The frame cap
	// above stops a single read costing 16 MiB of process memory, but the
	// contents of a read go somewhere the frame cap was never about: straight
	// into a model's context, chosen entirely by the server. Refused whole
	// rather than trimmed, for the reason the listings above are: a trimmed
	// resource is a partial file that reads like the file.
	maxResourceReadBytes = 1 << 20
	// maxDiagnosticsInError bounds how much recorded server chatter travels on
	// a returned error.
	maxDiagnosticsInError = 5
	// unroutablePreview bounds how much of an unroutable frame, or of a
	// recorded diagnostic, is quoted back. A server that emits megabytes of
	// junk must not have those megabytes copied into an error message.
	unroutablePreview = 200
)

// baseEnvAllowlist is the entire set of this process's environment variables an
// MCP child may inherit.
//
// It is an allowlist and not a denylist on purpose. cmd.Env used to be
// os.Environ(), so a weather server was handed the harness's model API key, its
// cloud credentials, its forge token and its SSH agent socket — 58 variables,
// of which it needed none. A denylist of credential names is wrong the moment
// anybody invents a new one; naming what may pass is wrong only for variables
// nobody thought of, and those fail loudly and get added here.
//
// Nothing on this list carries authority. Absent, and deliberately: every name
// in credentials.DefaultRequirements, SSH_AUTH_SOCK, the AWS and forge
// families, and the proxy variables — an http_proxy value routinely carries a
// username and password inside its URL.
var baseEnvAllowlist = []string{
	"PATH",
	"HOME",
	"USER",
	"LOGNAME",
	"SHELL",
	"LANG",
	"LANGUAGE",
	"LC_ALL",
	"LC_CTYPE",
	"LC_MESSAGES",
	"TZ",
	"TMPDIR",
	"TERM",
	// The Windows spellings of the same few things. They are simply absent on
	// unix, where looking them up finds nothing, so this needs no build tag.
	"SystemRoot",
	"SystemDrive",
	"windir",
	"COMSPEC",
	"PATHEXT",
	"TEMP",
	"TMP",
	"USERPROFILE",
	"APPDATA",
	"LOCALAPPDATA",
	"ProgramData",
	"NUMBER_OF_PROCESSORS",
	"OS",
}

// buildEnv constructs the child's entire environment explicitly.
//
// The result is never nil, and that matters more than it looks: os/exec reads a
// nil Env as "inherit everything", so a bug that produced no entries would
// quietly restore the leak this replaces instead of producing an empty
// environment.
func buildEnv(cfg ServerConfig, workdir string) []string {
	return buildEnvKeyed(cfg, workdir, childenv.Key)
}

// buildEnvKeyed is buildEnv with the name comparison supplied. On Windows a
// declared `Path` and an inherited `PATH` are one variable, and exec's
// last-wins rule would let the inherited one override the declared one if both
// were emitted.
func buildEnvKeyed(cfg ServerConfig, workdir string, nameKey func(string) string) []string {
	env := make([]string, 0, len(baseEnvAllowlist)+len(cfg.Env)+len(cfg.EnvPassthrough)+1)
	seen := make(map[string]bool)
	add := func(k, v string) {
		if k == "" || seen[nameKey(k)] {
			return
		}
		seen[nameKey(k)] = true
		env = append(env, k+"="+v)
	}

	// Declared values first: an explicit declaration outranks the inherited
	// default for the same name.
	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		add(k, cfg.Env[k])
	}
	for _, k := range cfg.EnvPassthrough {
		if v, ok := os.LookupEnv(k); ok {
			add(k, v)
		}
	}
	for _, k := range baseEnvAllowlist {
		if v, ok := os.LookupEnv(k); ok {
			add(k, v)
		}
	}
	if workdir != "" {
		add("PWD", workdir)
	}
	return env
}

// NewClient spawns a new MCP server subprocess and connects stdio pipes.
func NewClient(cfg ServerConfig) (*Client, error) {
	if cfg.Command == "" {
		return nil, errors.New("mcp: server command is required")
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}

	// The child's environment is constructed, never inherited. See buildEnv.
	cmd.Env = buildEnv(cfg, cmd.Dir)
	proc.ConfigureOwnGroup(cmd)
	cmd.WaitDelay = waitDelay

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: creating stdin pipe for %s: %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, fmt.Errorf("mcp: creating stdout pipe for %s: %w", cfg.Name, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, fmt.Errorf("mcp: creating stderr pipe for %s: %w", cfg.Name, err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, fmt.Errorf("mcp: starting process %s (%s): %w", cfg.Name, cfg.Command, err)
	}

	c := &Client{
		cfg:        cfg,
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		pending:    make(map[int64]chan *Response),
		doneCh:     make(chan struct{}),
		stderrDone: make(chan struct{}),
	}

	go c.readLoop()
	go c.stderrLoop()

	return c, nil
}

// stderrLoop records stderr diagnostics from the MCP server.
//
// It reads with the same readLimitedLine/drainToNewline pattern as stdout
// rather than with a bufio.Scanner. A Scanner stops *permanently* on
// ErrTooLong — it does not skip the token and carry on — so one oversized log
// line ended stderr draining for the life of the process: the child's stderr
// pipe filled, the child blocked in write(2) before it could answer, and every
// later call ran out its full timeout. And ErrTooLong was explicitly filtered
// out of the diagnostics, so a reader that had stopped reported exactly what a
// reader that ran clean reports — nothing. A check that could not run must
// never report the same result as a check that ran and passed, so the skip is
// recorded and draining continues.
func (c *Client) stderrLoop() {
	defer close(c.stderrDone)
	reader := bufio.NewReaderSize(c.stderr, 64*1024)
	for {
		line, err := readLimitedLine(reader, maxStderrLine)
		switch {
		case err == nil:
			c.recordError(strings.TrimRight(string(line), "\r\n"))
			continue
		case errors.As(err, new(errLineTooLarge)):
			c.recordError(fmt.Sprintf("stderr line skipped: %v", err))
			continue
		case errors.Is(err, io.EOF):
		default:
			c.recordError(fmt.Sprintf("stderr reader failed: %v", err))
		}
		return
	}
}

// recordError appends a diagnostic from one of the reader goroutines.
func (c *Client) recordError(msg string) {
	c.errMu.Lock()
	if len(c.serverErrors) < 100 {
		c.serverErrors = append(c.serverErrors, msg)
	}
	c.errMu.Unlock()
}

// Diagnostics returns what the server said about itself on stderr, plus the
// protocol violations the read loop recorded.
//
// It exists because this record had no reader at all. Both goroutines above
// have collected it since the client was written and nothing ever asked for
// it, so a server that explained its own failure on stderr — a missing
// interpreter, a rejected token, a stack trace — explained it to a slice that
// was discarded when the client was closed. The operator saw "server exited
// unexpectedly" and nothing else.
// awaitStderr waits until the stderr reader has returned, or until
// stderrDrainBound. A reader that already finished returns immediately.
func (c *Client) awaitStderr() {
	timer := time.NewTimer(stderrDrainBound)
	defer timer.Stop()
	select {
	case <-c.stderrDone:
	case <-timer.C:
	}
}

// awaitExit waits for the read loop to finish, which includes draining stderr,
// for at most stderrDrainBound or until ctx ends. A server that is still alive
// after its stdin broke is not waited on past that bound.
func (c *Client) awaitExit(ctx context.Context) {
	timer := time.NewTimer(stderrDrainBound)
	defer timer.Stop()
	select {
	case <-c.doneCh:
	case <-ctx.Done():
	case <-timer.C:
	}
}

func (c *Client) Diagnostics() []string {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return append([]string(nil), c.serverErrors...)
}

// diagnosticsSuffix renders the tail of that record for an error message. This
// is the reader that matters: a failure the server already explained must
// arrive carrying the explanation.
func (c *Client) diagnosticsSuffix() string {
	diags := c.Diagnostics()
	if len(diags) == 0 {
		return ""
	}
	if len(diags) > maxDiagnosticsInError {
		diags = diags[len(diags)-maxDiagnosticsInError:]
	}
	for i, d := range diags {
		diags[i] = truncateForMessage(d)
	}
	return " — the server reported: " + strings.Join(diags, " | ")
}

func truncateForMessage(s string) string {
	return truncateTo(strings.TrimSpace(s), unroutablePreview)
}

// truncateTo cuts s to at most n bytes without leaving a partial rune behind.
func truncateTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := s[:n]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// Alive reports whether the server process is still producing stdout. A client
// whose read loop has ended is dead regardless of whether anyone called Close,
// and a closed client is dead before its read loop has finished noticing: the
// Manager uses this to replace a crashed or retired server instead of handing
// out a corpse to every later call.
func (c *Client) Alive() bool {
	if c.closed.Load() {
		return false
	}
	select {
	case <-c.doneCh:
		return false
	default:
		return true
	}
}

// Done exposes the channel closed when the server's stdout ends.
func (c *Client) Done() <-chan struct{} { return c.doneCh }

// readLimitedLine returns one line including its trailing newline, if it had
// one. The final unterminated line at EOF counts as a line, matching the
// bufio.Scanner semantics this replaced. A line longer than limit is drained
// to its terminator and reported as errLineTooLarge rather than ending the
// stream: one runaway frame must not convert a healthy session into permanent
// tool failure.
//
// The prefix is kept because whether an over-cap frame was a JSON-RPC frame or
// server chatter decides who has to hear about it. `xxxxx…` on stdout is a
// noisy server and the session carries on; a 17 MiB line that opens with `{`
// was a reply, and the request it was a reply to must be failed rather than
// left to sit out its full timeout on a response that is never coming.
// It carries the limit it exceeded as well, because both streams use this
// reader with different bounds; naming maxStdoutLine unconditionally reported
// the wrong number for a skipped stderr line.
type errLineTooLarge struct {
	prefix []byte
	limit  int
}

func (e errLineTooLarge) Error() string {
	return fmt.Sprintf("server emitted a line exceeding %d bytes", e.limit)
}

func readLimitedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		switch {
		case err == nil:
			if buf == nil {
				// The line fit in the buffer, so the window ReadSlice returned
				// is handed back uncopied. It is valid until the next read, and
				// both readers finish with a line before reading again:
				// dispatch decodes into fresh values and stderr converts to a
				// string.
				return chunk, nil
			}
			return append(buf, chunk...), nil
		case errors.Is(err, bufio.ErrBufferFull):
			buf = append(buf, chunk...)
			if len(buf) > limit {
				drainToNewline(r)
				prefix := buf
				if len(prefix) > unroutablePreview {
					prefix = prefix[:unroutablePreview]
				}
				return nil, errLineTooLarge{prefix: append([]byte(nil), prefix...), limit: limit}
			}
		case err == io.EOF:
			if len(buf)+len(chunk) > 0 {
				return append(buf, chunk...), nil
			}
			return nil, io.EOF
		default:
			return append(buf, chunk...), err
		}
	}
}

// drainToNewline consumes through the next newline so a skipped oversized
// frame cannot leave its tail to be parsed as phantom responses.
func drainToNewline(r *bufio.Reader) {
	for {
		_, err := r.ReadSlice('\n')
		if err == nil || (err != nil && !errors.Is(err, bufio.ErrBufferFull)) {
			return
		}
	}
}

// readLoop processes incoming JSON-RPC lines from the server.
func (c *Client) readLoop() {
	defer close(c.doneCh)
	reader := bufio.NewReaderSize(c.stdout, 64*1024)
	for {
		line, err := readLimitedLine(reader, maxStdoutLine)
		var tooLarge errLineTooLarge
		switch {
		case err == nil:
		case errors.As(err, &tooLarge):
			c.recordError(err.Error())
			// A frame we deliberately did not read cannot be routed. If it
			// opened like a JSON-RPC frame it was somebody's reply, and the
			// caller waiting for that reply is told now rather than at the far
			// end of a two-minute timeout.
			if looksLikeRPCFrame(tooLarge.prefix) {
				c.failAllPending(fmt.Sprintf("%s and it was discarded unread; "+
					"the reply to this request cannot be recovered", err.Error()))
			}
			continue
		case errors.Is(err, io.EOF):
		default:
			c.recordError(fmt.Sprintf("stdout reader failed: %v", err))
		}
		if err != nil {
			break
		}
		if len(line) == 0 {
			continue
		}
		c.dispatch(line)
	}

	// stdout has ended. The server's explanation of that ending is often
	// still in the stderr pipe. Failing the pending calls before it is read
	// reports a bare close for a death the server already explained.
	c.awaitStderr()

	// Flush all remaining pending requests on EOF
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.pendingMu.Unlock()
}

// dispatch routes one stdout frame, and decides what to do with the ones it
// cannot route.
//
// The rule this replaces was a bare `continue` on every frame that did not
// unmarshal or did not carry a numeric id, which is how a hostile or merely
// broken server wedged a call for its full 120 seconds without anything
// anywhere recording that it had happened. The reply never arrived, the pending
// entry was never failed, and the diagnostic — if one was written at all — went
// to a slice with no reader.
//
// So frames are now classified rather than skipped:
//
//   - Carries a "method": a notification or a server-initiated request. Not a
//     reply to anything, correctly ignored.
//   - Routes to a pending id: delivered.
//   - Numeric id that is not pending: a late reply to a call that already
//     timed out or was cancelled. Normal, recorded, tolerated — failing live
//     calls over one of these would punish the wrong request.
//   - Not JSON-RPC shaped at all: server chatter on stdout, which plenty of
//     real servers emit. Recorded, tolerated.
//   - Anything else — unparseable but object-shaped, or reply-shaped with an
//     id this client cannot match — is a protocol violation. Correlation is no
//     longer trustworthy, so every in-flight call is failed with the reason.
//     The client stays alive; the ids it issues next are fresh, and the
//     Manager replaces it if the stream really is dead.
func (c *Client) dispatch(line []byte) {
	resp, method, hasPayload, err := decodeFrame(line)
	switch {
	case errors.Is(err, errUnreadableFrame):
		c.recordError("ignored unreadable frame: " + truncateForMessage(string(line)))
		return
	case err != nil:
		if looksLikeRPCFrame(line) {
			reason := fmt.Sprintf("server %s emitted a frame that is not valid JSON-RPC: %v",
				c.Name(), err)
			c.recordError(reason)
			c.failAllPending(reason)
			return
		}
		c.recordError("ignored non-JSON output on stdout: " + truncateForMessage(string(line)))
		return
	}
	if method != "" {
		c.answerServerFrame(line, method)
		return
	}
	if !hasPayload && resp.JSONRPC == "" {
		// A JSON object that is not claiming to be a JSON-RPC reply — a server
		// logging structured lines to stdout. Not ours to fail calls over.
		c.recordError("ignored non-JSON-RPC object on stdout: " + truncateForMessage(string(line)))
		return
	}

	id, ok := numericID(resp.ID)
	if !ok && resp.ID == nil && resp.Error != nil {
		// The server could not read a request well enough to know whose it
		// was, and answers with an explicit "id": null (DevCouncil's server
		// since wave 6: a parse error, or -32600 for a frame over its size
		// bound). It is the server's own account of why the request failed, so
		// it is what the in-flight calls fail with — not "an id this client
		// cannot match", which names the envelope and drops the cause.
		reason := fmt.Sprintf("server %s could not read a request: %s (code %d)",
			c.Name(), truncateForMessage(resp.Error.Message), resp.Error.Code)
		c.recordError(reason)
		c.failAllPending(reason)
		return
	}
	if !ok {
		reason := fmt.Sprintf("server %s replied with an id this client cannot match (%s); "+
			"every request it issued uses a JSON number", c.Name(), renderID(resp.ID))
		c.recordError(reason)
		c.failAllPending(reason)
		return
	}

	c.pendingMu.Lock()
	ch, waiting := c.pending[id]
	if waiting {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()

	if !waiting || ch == nil {
		c.recordError(fmt.Sprintf("discarded a reply to request %d, which is no longer pending", id))
		return
	}
	ch <- &resp
}

// answerServerFrame replies to a frame the server initiated. A notification
// (no id) needs nothing. A request (an id, any JSON type) must be answered by
// every party, or the server waits on it forever: "ping" gets the empty
// result the ping utility specifies, and every other method gets -32601 —
// roots/list included, because this client does not declare the roots
// capability, and sampling/elicitation are not offered either. The id is
// echoed byte for byte, so a string or large numeric id survives.
//
// Spec: modelcontextprotocol.io/specification/2025-11-25/basic/utilities/ping
// (the receiver MUST respond promptly with an empty response) and
// .../client/roots (a server may only call roots/list on a client that
// declared the capability).
func (c *Client) answerServerFrame(line []byte, method string) {
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(line, &probe) != nil || len(probe.ID) == 0 || string(probe.ID) == "null" {
		return
	}
	reply := struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  *struct{}       `json:"result,omitempty"`
		Error   *RPCError       `json:"error,omitempty"`
	}{JSONRPC: JSONRPCVersion, ID: probe.ID}
	if method == "ping" {
		reply.Result = &struct{}{}
	} else {
		reply.Error = &RPCError{Code: CodeMethodNotFound, Message: "method not supported by this client: " + truncateForMessage(method)}
	}
	data, err := json.Marshal(reply)
	if err == nil {
		err = c.writeFrame(append(data, '\n'))
	}
	if err != nil {
		c.recordError(fmt.Sprintf("could not answer server request %q: %v", truncateForMessage(method), err))
	}
}

// errUnreadableFrame is a frame that decodes as a Response but whose envelope
// does not — a "method" that is not a string.
var errUnreadableFrame = errors.New("mcp: unreadable frame")

// decodeFrame reads one stdout frame: the reply it carries, its method if it
// is a notification or request, and whether it has a "result" or "error"
// member at all. A non-nil error other than errUnreadableFrame is the strict
// decode's own, which is what a violation is reported with.
//
// Frames can be tool results of many megabytes, so the common case is one
// decode into the reply and its method together, taken when it succeeds and
// the frame carries a result or an error. Everything else is small — a
// notification, a log line, a malformed frame — and takes the original two
// decodes: the combined one names its errors differently, and cannot tell an
// explicit "error": null from an absent one.
func decodeFrame(line []byte) (resp Response, method string, hasPayload bool, err error) {
	var frame struct {
		Response
		Method string `json:"method"`
	}
	if json.Unmarshal(line, &frame) == nil && (len(frame.Result) > 0 || frame.Error != nil) {
		return frame.Response, frame.Method, true, nil //nolint:nilerr // reached only when the decode succeeded
	}

	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, "", false, err
	}
	var envelope struct {
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return Response{}, "", false, errUnreadableFrame
	}
	return resp, envelope.Method, len(envelope.Result) > 0 || len(envelope.Error) > 0, nil
}

// failAllPending fails every in-flight request with reason.
//
// The reason travels as an RPCError rather than by closing the channel, so the
// caller's error names what actually went wrong instead of the generic "closed
// connection before replying" a closed channel produces.
func (c *Client) failAllPending(reason string) {
	c.pendingMu.Lock()
	pending := c.pending
	c.pending = make(map[int64]chan *Response)
	c.pendingMu.Unlock()

	for _, ch := range pending {
		if ch == nil {
			continue
		}
		// Every channel is buffered with room for one and is read at most
		// once, so this cannot block.
		ch <- &Response{
			JSONRPC:   JSONRPCVersion,
			Error:     &RPCError{Code: CodeInternalError, Message: reason},
			synthetic: true,
		}
	}
}

// looksLikeRPCFrame reports whether these bytes opened like a JSON object, and
// so were a frame this client was meant to parse rather than server chatter.
func looksLikeRPCFrame(line []byte) bool {
	trimmed := strings.TrimLeft(string(line), " \t\r\n")
	return strings.HasPrefix(trimmed, "{")
}

// numericID resolves a JSON-RPC id to the integer this client issued.
//
// A string id is spec-legal, and a server that echoes `"7"` for the request
// sent as `7` is answering it. That is accepted. What is not accepted is an id
// this client could not have issued: it means the reply cannot be attributed,
// which is a protocol violation and not something to skip past.
func numericID(v any) (int64, bool) {
	switch id := v.(type) {
	case float64:
		if id != float64(int64(id)) {
			return 0, false
		}
		return int64(id), true
	case int64:
		return id, true
	case int:
		return int64(id), true
	case json.Number:
		n, err := id.Int64()
		return n, err == nil
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(id), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

func renderID(v any) string {
	if v == nil {
		return "null"
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return truncateForMessage(string(raw))
}

// Call performs a synchronous JSON-RPC request/response roundtrip.
//
// A caller context without a deadline is bounded by cfg.Timeout, or
// defaultCallTimeout when that is unset: an unbounded wait on a wedged server
// would freeze the whole tool surface, and nothing in this protocol is worth
// waiting forever for.
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callCeiling())
		defer cancel()
	}

	id, respCh, err := c.send(ctx, method, params)
	if err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		c.forget(id)
		c.cancelRequest(id, method, ctx.Err())
		// Whatever the server said about itself travels with the timeout. A
		// wedge whose cause was recorded on stderr and then discarded is how
		// "the tool call hung" became the whole of an operator's evidence.
		return fmt.Errorf("mcp: %s on %s: %w%s", method, c.cfg.Name, ctx.Err(), c.diagnosticsSuffix())
	case <-c.doneCh:
		return fmt.Errorf("mcp: server %s exited unexpectedly%s", c.cfg.Name, c.diagnosticsSuffix())
	case resp, ok := <-respCh:
		return c.replyResult(method, resp, ok, result)
	}
}

// callCeiling is the bound on a wait whose caller set none.
func (c *Client) callCeiling() time.Duration {
	if c.cfg.Timeout > 0 {
		return c.cfg.Timeout
	}
	return defaultCallTimeout
}

// send registers a pending entry for a new request and writes it. It is the
// half of Call that does not wait, so the handshake can issue a request whose
// life is not tied to any one caller's context. ctx only bounds the post-failure
// wait for the server's explanation; it does not outlive the write.
func (c *Client) send(ctx context.Context, method string, params any) (int64, chan *Response, error) {
	if c.closed.Load() {
		return 0, nil, c.closedReason()
	}
	if !c.Alive() {
		return 0, nil, fmt.Errorf("mcp: server %s has exited%s", c.cfg.Name, c.diagnosticsSuffix())
	}

	id := c.reqCounter.Add(1)

	var paramsRaw json.RawMessage
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return 0, nil, fmt.Errorf("mcp: encoding params for %s: %w", method, err)
		}
		paramsRaw = raw
	}

	req := Request{
		JSONRPC: JSONRPCVersion,
		ID:      id,
		Method:  method,
		Params:  paramsRaw,
	}

	data, err := json.Marshal(req)
	if err != nil {
		return 0, nil, fmt.Errorf("mcp: encoding request for %s: %w", method, err)
	}
	data = append(data, '\n')

	respCh := make(chan *Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	if writeErr := c.writeFrame(data); writeErr != nil {
		c.forget(id)
		// A write that fails because the child stopped reading is usually a
		// child that is exiting, and what it said on the way out is still in
		// flight. Returning now made the same failure read differently
		// depending on whether stderr had been drained yet.
		c.awaitExit(ctx)
		return 0, nil, fmt.Errorf("mcp: writing request %s to %s: %w%s", method, c.cfg.Name, writeErr, c.diagnosticsSuffix())
	}
	return id, respCh, nil
}

// forget drops a request's pending entry, after which a reply to it is recorded
// by dispatch as "no longer pending" and delivered nowhere.
func (c *Client) forget(id int64) {
	c.pendingMu.Lock()
	delete(c.pending, id)
	c.pendingMu.Unlock()
}

// replyResult turns what arrived on a request's channel into Call's result. A
// closed channel (ok false) is the read loop flushing it at end of stream.
func (c *Client) replyResult(method string, resp *Response, ok bool, result any) error {
	if !ok || resp == nil {
		return fmt.Errorf("mcp: server %s closed connection before replying to %s%s",
			c.cfg.Name, method, c.diagnosticsSuffix())
	}
	if resp.Error != nil {
		return &serverError{server: c.cfg.Name, rpc: resp.Error}
	}
	if result != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, result); err != nil {
			return fmt.Errorf("mcp: unmarshaling %s result from %s: %w", method, c.cfg.Name, err)
		}
	}
	return nil
}

// cancelRequest tells the server a request this client issued is abandoned.
// On stdio there is no per-request stream to close, so the spec requires a
// notifications/cancelled naming the request id; without it a non-idempotent
// tool keeps running server-side while the caller is told it failed and may
// retry it. "initialize" is never cancelled (the spec forbids it). A failed
// write is recorded, not returned: the caller's own timeout/cancel error is
// the one that matters, and a late reply is already tolerated by dispatch.
//
// Spec: modelcontextprotocol.io/specification/2025-11-25/basic/utilities/cancellation
func (c *Client) cancelRequest(id int64, method string, cause error) {
	if method == "initialize" || c.closed.Load() {
		return
	}
	params := map[string]any{"requestId": id}
	if cause != nil {
		params["reason"] = cause.Error()
	}
	if err := c.Notify("notifications/cancelled", params); err != nil {
		c.recordError(fmt.Sprintf("could not cancel request %d (%s): %v", id, method, err))
	}
}

// ErrWriteUncertain reports a frame this client could not establish the fate
// of: the write outlived writeTimeout, so an unknown prefix of it — possibly
// all of it — is already in the child's pipe.
//
// It is a distinct type because the distinction is the whole point. A caller
// that treats it as "the request was not sent" and retries will run a
// non-idempotent tool a second time. There is no way for this layer to
// discover which happened, so it names the ambiguity instead of resolving it
// one way and being wrong half the time.
type ErrWriteUncertain struct {
	// Server is the configured server name, for the message.
	Server string
	// After is the bound the write outlived.
	After time.Duration
}

func (e *ErrWriteUncertain) Error() string {
	return fmt.Sprintf("stdin write to %s did not complete within %s: the request may already have "+
		"been delivered and executed, so it must not be retried blindly; the connection was torn down",
		e.Server, e.After)
}

// writeFrame writes one newline-terminated frame under the writer lock, with
// the write itself bounded: a child that stopped draining its stdin would
// otherwise hold writerMu forever and freeze every other call in the queue.
//
// A timed-out write ends the connection rather than being retried past. The
// bound is enforced by giving up on the write, not by cancelling it — the
// goroutine is still inside stdin.Write when writeFrame returns, and it may
// have already delivered part of the frame into the pipe buffer. Releasing
// writerMu at that point let the next call append a second frame behind a
// partial first one: two JSON documents spliced into one line, which the
// server reads as one malformed request and which no reply ever arrives for.
// So the wedge latches, every later write is refused with the reason, and
// stdin is closed on the way out — that unblocks the stranded goroutine and
// releases the frame it was holding, which is also what stops one leaked
// goroutine and buffer accumulating per timed-out call.
func (c *Client) writeFrame(data []byte) error {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()

	if c.stdin == nil {
		return errors.New("stdin is not connected")
	}
	if c.writeWedged.Load() {
		return fmt.Errorf("stdin is no longer usable: an earlier write to %s did not complete "+
			"within %s, so this connection may carry a partial frame and was retired",
			c.cfg.Name, writeTimeout)
	}

	errCh := make(chan error, 1)
	go func() {
		_, werr := c.stdin.Write(data)
		errCh <- werr
	}()

	select {
	case werr := <-errCh:
		return werr
	case <-time.After(writeTimeout):
		c.writeWedged.Store(true)
		// Close, not just stdin.Close: shutting stdin unblocks the stranded
		// goroutine and bounds how much of the frame can ever land, but the
		// child also has to be reaped and the read loop ended, or a wedged
		// connection leaves a live server process behind it. Close does not
		// take writerMu, so calling it under that lock is safe.
		_ = c.Close()
		// A typed error because "not delivered" and "unknown" are different
		// facts and only the second one is true here: the goroutine may already
		// have delivered the frame into the pipe buffer before the deadline, so
		// the server may have executed a request this caller is being told
		// failed. Callers that must not retry blind can test for it.
		return &ErrWriteUncertain{Server: c.Name(), After: writeTimeout}
	}
}

// closedReason explains why this client is closed.
//
// "Closed" and "retired after a wedged write" are different facts and a caller
// has to be able to tell them apart: the first is an orderly shutdown, the
// second means a frame may be half-written on a stream nobody will ever read,
// and a caller that retries into it corrupts the next request too. Close is
// what a wedged write calls to reap the child, so without this the more
// specific fact was overwritten by the more general one.
func (c *Client) closedReason() error {
	if c.writeWedged.Load() {
		return fmt.Errorf("mcp: client is closed: an earlier write to %s did not complete within %s, "+
			"so the connection was retired because it may carry a partial frame",
			c.cfg.Name, writeTimeout)
	}
	return errors.New("mcp: client is closed")
}

// Notify sends a one-way notification without waiting for a response.
func (c *Client) Notify(method string, params any) error {
	if c.closed.Load() {
		return c.closedReason()
	}
	if !c.Alive() {
		return fmt.Errorf("mcp: server %s has exited", c.cfg.Name)
	}

	var paramsRaw json.RawMessage
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("mcp: encoding notification params for %s: %w", method, err)
		}
		paramsRaw = raw
	}

	notif := Notification{
		JSONRPC: JSONRPCVersion,
		Method:  method,
		Params:  paramsRaw,
	}

	data, err := json.Marshal(notif)
	if err != nil {
		return fmt.Errorf("mcp: encoding notification %s: %w", method, err)
	}
	data = append(data, '\n')

	return c.writeFrame(data)
}

// initFlight is one handshake in progress. Its request is not owned by any
// caller: it ends when the server answers, when the transport ends, or when the
// client is closed.
type initFlight struct {
	done      chan struct{} // closed once res and err are final
	abort     chan struct{} // closed by Close
	abortOnce sync.Once
	res       *InitializeResult
	err       error
}

func (f *initFlight) abortNow() { f.abortOnce.Do(func() { close(f.abort) }) }

// Initialize performs the MCP handshake, once per session, however many callers
// ask.
//
// The initialize request is single-flight and belongs to the client, not to the
// first caller: it is never cancelled (the spec forbids cancelling it, and
// Call's cancelRequest skips it), so a caller whose context ends does not
// abandon it. It stays pending until the server answers, the server's stdout
// ends or the client is closed, and a reply that arrives after every caller has
// gone is still adopted — validated, recorded, and answered with exactly one
// notifications/initialized — because a server that has answered considers the
// session initialized whether or not anybody was waiting. Callers wait on the
// shared result under their own context (or, with none, the call ceiling), and
// one whose context ends gets its own error without disturbing the handshake.
//
// How a handshake ends decides whether it may be tried again. initialize is
// never re-sent while a previous one may still be outstanding or answered at
// the server: two initialize requests on one session is a protocol violation,
// and a late reply could be matched to the wrong attempt.
//
//   - A JSON-RPC error reply to our own initialize id is the server's definitive
//     "no". The flight is cleared and the next call starts a new handshake. The
//     one exception is -32602 with `data.supported`: handshake sends initialize
//     once more, on a new id of the same flight, at the newest revision both
//     sides speak (see handshake).
//   - A transport that ended is already dead: Alive is false and every later
//     call fails there.
//   - Anything that leaves the server's view of the session unknown or
//     inconsistent with ours is terminal for this client: the request was failed
//     by failAllPending (correlation lost, so the server may still answer it), a
//     success reply carried a protocol revision this client does not speak, a
//     success reply could not be decoded, or notifications/initialized could not
//     be written. Every waiter gets a named error, Initialize keeps returning
//     it, no further initialize is sent, and the client is closed so that
//     Alive is false and the Manager replaces it with a fresh process, which is
//     a fresh session.
//
// Spec: modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle (the
// client MUST send initialized after a successful initialize; if it does not
// support the version in the server's response it SHOULD disconnect) and
// .../basic/utilities/cancellation (the initialize request MUST NOT be
// cancelled by clients).
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	c.initMu.Lock()
	switch {
	case c.initResult != nil:
		res := c.initResult
		c.initMu.Unlock()
		return res, nil
	case c.initFatal != nil:
		err := c.initFatal
		c.initMu.Unlock()
		return nil, err
	}
	f := c.initFlight
	if f == nil {
		f = &initFlight{done: make(chan struct{}), abort: make(chan struct{})}
		c.initFlight = f
		// Detached from the caller: the handshake outlives it. WithoutCancel keeps
		// the context's values and drops its cancellation and deadline.
		go c.runInitialize(context.WithoutCancel(ctx), f)
	}
	c.initMu.Unlock()

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.callCeiling())
		defer cancel()
	}
	select {
	case <-f.done:
		return f.res, f.err
	case <-ctx.Done():
		return nil, fmt.Errorf("mcp: initialize on %s: %w%s", c.cfg.Name, ctx.Err(), c.diagnosticsSuffix())
	}
}

// runInitialize drives one flight to its end and publishes the outcome.
func (c *Client) runInitialize(ctx context.Context, f *initFlight) {
	res, err := c.handshake(ctx, f)
	var end *handshakeEnded
	fatal := errors.As(err, &end)

	c.initMu.Lock()
	switch {
	case err == nil:
		c.initResult = res
	case fatal:
		c.initFatal = err
	}
	c.initFlight = nil
	c.initMu.Unlock()

	f.res, f.err = res, err
	close(f.done)

	// After the waiters are released: Close waits on the child process.
	if fatal {
		_ = c.Close()
	}
}

// handshakeEnded marks a handshake that ended in a way that leaves the
// server's view of the session unknown or inconsistent with ours. It is
// terminal for the client, which is closed, so that the Manager replaces it with
// a fresh process and so a fresh session. It unwraps, so callers see the
// underlying error unchanged.
type handshakeEnded struct{ err error }

func (e *handshakeEnded) Error() string { return e.err.Error() }
func (e *handshakeEnded) Unwrap() error { return e.err }

// handshake sends initialize, waits for its resolution however long that takes,
// and completes the handshake. A failure is retryable unless it is a
// *handshakeEnded. See Initialize for which is which.
//
// It asks for ProtocolVersionLatest. A server that supports only older revisions
// may answer with a counter-offer, which is a success reply, or with the error
// the lifecycle page's own example shows: -32602 with `data.supported`. For
// that error, and only that error, the request is sent once more with the
// newest revision both sides speak: one outstanding initialize at a time (the
// first has been answered), a new request id on the same flight, and
// notifications/initialized only after the second one succeeds. The second
// attempt's outcome is final, so this never loops.
//
// Spec: modelcontextprotocol.io/specification/2025-11-25/basic/lifecycle,
// "Version Negotiation": the server "MUST respond with another protocol version
// it supports" and the error example carries `supported` and `requested`.
func (c *Client) handshake(ctx context.Context, f *initFlight) (*InitializeResult, error) {
	res, err := c.initializeAs(ctx, f, ProtocolVersionLatest)
	var rpcErr *RPCError
	var end *handshakeEnded
	if err == nil || errors.As(err, &end) || !errors.As(err, &rpcErr) || rpcErr.Code != CodeInvalidParams {
		return res, err
	}
	var data struct {
		Supported []string `json:"supported"`
	}
	if json.Unmarshal(rpcErr.Data, &data) != nil || len(data.Supported) == 0 {
		return res, err
	}
	var common []string
	for _, v := range data.Supported {
		if slices.Contains(handshakeVersions, v) {
			common = append(common, v)
		}
	}
	if len(common) == 0 {
		return nil, fmt.Errorf("mcp: %s supports protocol revision(s) %s and this client speaks %s, so there is no revision to agree on: %w",
			c.cfg.Name, strings.Join(data.Supported, ", "), strings.Join(handshakeVersions, ", "), err)
	}
	newest := slices.Max(common) // ISO dates order lexicographically
	if newest == ProtocolVersionLatest {
		// The server lists what it was just asked for and refused it anyway:
		// nothing a second request would change.
		return res, err
	}
	return c.initializeAs(ctx, f, newest)
}

// initializeAs is one initialize request at the given revision and what follows
// a success: the checks on the reply and notifications/initialized.
func (c *Client) initializeAs(ctx context.Context, f *initFlight, version string) (*InitializeResult, error) {
	params := InitializeParams{
		ProtocolVersion: version,
		// No roots capability: nothing here serves roots/list, and a server
		// told otherwise would wait on a list that never comes. Roots is also
		// deprecated as of protocol 2026-07-28.
		Capabilities: ClientCapabilities{},
		ClientInfo: Implementation{
			Name:    "manvi",
			Version: "1.0.0",
		},
	}

	id, respCh, err := c.send(ctx, "initialize", params)
	if err != nil {
		return nil, err
	}

	var resp *Response
	var ok bool
	select {
	case resp, ok = <-respCh:
	case <-c.doneCh:
		// A reply that landed in the same instant is still a reply.
		select {
		case resp, ok = <-respCh:
		default:
			return nil, fmt.Errorf("mcp: server %s exited unexpectedly%s", c.cfg.Name, c.diagnosticsSuffix())
		}
	case <-f.abort:
		c.forget(id)
		return nil, c.closedReason()
	}

	if ok && resp != nil && resp.synthetic {
		// Not the server's answer: failAllPending, because correlation on this
		// stream was lost. The server may still hold our initialize and may
		// still answer it.
		return nil, &handshakeEnded{fmt.Errorf("mcp: the initialize request to %s was failed by this client, not answered by the server (%s); "+
			"the session is in an unknown state, so the connection was closed and initialize will not be sent again on it",
			c.cfg.Name, resp.Error.Message)}
	}
	var out InitializeResult
	if err := c.replyResult("initialize", resp, ok, &out); err != nil {
		if ok && resp != nil && resp.Error == nil {
			// The server answered with success and this client cannot read it:
			// the server considers the session initialized.
			return nil, &handshakeEnded{err}
		}
		return nil, err
	}
	if !slices.Contains(handshakeVersions, out.ProtocolVersion) {
		// The server accepted the handshake on a revision this client does not
		// speak, so its session is established on terms this client cannot
		// honour. "If the client does not support the version in the server's
		// response, it SHOULD disconnect."
		return nil, &handshakeEnded{fmt.Errorf("mcp: %s answered initialize with protocol revision %q, which this client does not speak (it speaks %s)",
			c.cfg.Name, out.ProtocolVersion, strings.Join(handshakeVersions, ", "))}
	}

	if err := c.Notify("notifications/initialized", map[string]any{}); err != nil {
		return nil, &handshakeEnded{fmt.Errorf("mcp: notifying initialization to %s: %w", c.cfg.Name, err)}
	}
	return &out, nil
}

// ListTools returns all tools offered by the MCP server, after checking that
// the listing is within the bounds a listing has to be within.
//
// It is refused whole rather than trimmed. A trimmed listing is a capped sample
// presented as the server's full offering, and every caller downstream — the
// model included — would read it as complete.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if _, err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	var res ListToolsResult
	if err := c.Call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	if err := c.checkToolListing(res.Tools); err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// checkToolListing bounds an attacker-controlled tools/list reply.
func (c *Client) checkToolListing(list []Tool) error {
	if len(list) > maxToolsPerServer {
		return fmt.Errorf("mcp: server %s advertised %d tools, past the %d cap; "+
			"the listing was refused rather than truncated", c.Name(), len(list), maxToolsPerServer)
	}
	for i, tool := range list {
		switch {
		case tool.Name == "":
			return fmt.Errorf("mcp: server %s advertised a tool at index %d with no name", c.Name(), i)
		case len(tool.Name) > maxToolNameLen:
			return fmt.Errorf("mcp: server %s advertised a tool name of %d bytes, past the %d cap",
				c.Name(), len(tool.Name), maxToolNameLen)
		case hasControlChars(tool.Name):
			// A newline or an escape sequence in a name is not a name. It is
			// an attempt to forge structure in whatever renders the listing.
			return fmt.Errorf("mcp: server %s advertised a tool name containing control characters: %q",
				c.Name(), truncateForMessage(tool.Name))
		case len(tool.Description) > maxToolDescLen:
			return fmt.Errorf("mcp: server %s advertised a %d-byte description for tool %q, past the %d cap",
				c.Name(), len(tool.Description), tool.Name, maxToolDescLen)
		case len(tool.InputSchema) > maxToolSchemaBytes:
			return fmt.Errorf("mcp: server %s advertised a %d-byte input schema for tool %q, past the %d cap",
				c.Name(), len(tool.InputSchema), tool.Name, maxToolSchemaBytes)
		}
	}
	return nil
}

// hasControlChars reports whether s carries a C0 control character or DEL.
func hasControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// CallTool executes a tool on the MCP server.
func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (*CallToolResult, error) {
	if _, err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	params := CallToolParams{
		Name:      name,
		Arguments: arguments,
	}

	var res CallToolResult
	if err := c.Call(ctx, "tools/call", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// ListResources returns resources exposed by the MCP server.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	if _, err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	var res ListResourcesResult
	if err := c.Call(ctx, "resources/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	if len(res.Resources) > maxResourcesPerServer {
		return nil, fmt.Errorf("mcp: server %s advertised %d resources, past the %d cap; "+
			"the listing was refused rather than truncated",
			c.Name(), len(res.Resources), maxResourcesPerServer)
	}
	for i, r := range res.Resources {
		if len(r.URI) > maxResourceFieldLen || len(r.Name) > maxResourceFieldLen {
			return nil, fmt.Errorf("mcp: server %s advertised a resource at index %d whose uri or name "+
				"is past the %d-byte cap", c.Name(), i, maxResourceFieldLen)
		}
		if hasControlChars(r.URI) || hasControlChars(r.Name) {
			return nil, fmt.Errorf("mcp: server %s advertised a resource at index %d whose uri or name "+
				"contains control characters", c.Name(), i)
		}
	}
	return res.Resources, nil
}

// ReadResource reads content of an MCP resource by URI.
func (c *Client) ReadResource(ctx context.Context, uri string) ([]ResourceContent, error) {
	if _, err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	params := ReadResourceParams{URI: uri}
	var res ReadResourceResult
	if err := c.Call(ctx, "resources/read", params, &res); err != nil {
		return nil, err
	}
	total := 0
	for _, content := range res.Contents {
		total += len(content.Text) + len(content.Blob)
	}
	if total > maxResourceReadBytes {
		return nil, fmt.Errorf("mcp: server %s returned %d bytes for resource %q, past the %d-byte cap; "+
			"the read was refused rather than truncated",
			c.Name(), total, uri, maxResourceReadBytes)
	}
	return res.Contents, nil
}

// Close gracefully terminates the MCP server process.
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}

	// Release a handshake in flight and everybody waiting on it. closed is set
	// first, so a flight that starts after this read fails in send.
	c.initMu.Lock()
	f := c.initFlight
	c.initMu.Unlock()
	if f != nil {
		f.abortNow()
	}

	// Close stdin to signal EOF to server
	if c.stdin != nil {
		_ = c.stdin.Close()
	}

	// Wait briefly for clean exit
	done := make(chan struct{})
	go func() {
		if c.cmd != nil && c.cmd.Process != nil {
			_, _ = c.cmd.Process.Wait()
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		if c.cmd != nil && c.cmd.Process != nil {
			// The group first, then the leader. Killing only the leader left
			// whatever it had spawned running — a proxy, a language runtime, a
			// watcher — holding the pipes it inherited and outliving the
			// session that started it. The group is the server's own, so this
			// cannot reach anything else this harness is running.
			proc.KillGroup(c.cmd.Process.Pid)
			_ = c.cmd.Process.Kill()
		}
		// A kill without a reap leaves the wait-goroutine running and a
		// zombie on the table; give it a bounded second chance.
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}

	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	if c.stderr != nil {
		_ = c.stderr.Close()
	}

	return nil
}

// Name returns the configured server name.
func (c *Client) Name() string {
	if c.cfg.Name != "" {
		return c.cfg.Name
	}
	return c.cfg.Command
}
