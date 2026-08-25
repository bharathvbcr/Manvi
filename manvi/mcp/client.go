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
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

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

	initMu       sync.Mutex
	initResult   *InitializeResult
	initErr      error
	initTried    bool
	closed       atomic.Bool
	doneCh       chan struct{}
	serverErrors []string
	errMu        sync.Mutex
}

const (
	// maxStdoutLine bounds one JSON-RPC frame from the server. A line past it
	// is skipped with a recorded diagnostic rather than allowed to end the
	// session: the next line may be perfectly good.
	maxStdoutLine = 16 << 20
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
	env := make([]string, 0, len(baseEnvAllowlist)+len(cfg.Env)+len(cfg.EnvPassthrough)+1)
	seen := make(map[string]bool)
	add := func(k, v string) {
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
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
	setOwnProcessGroup(cmd)
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
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: make(map[int64]chan *Response),
		doneCh:  make(chan struct{}),
	}

	go c.readLoop()
	go c.stderrLoop()

	return c, nil
}

// stderrLoop records stderr diagnostics from the MCP server.
func (c *Client) stderrLoop() {
	scanner := bufio.NewScanner(c.stderr)
	// Diagnostics are worth keeping only while they are small; a server
	// emitting megabyte log lines must not grow this buffer without bound.
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		c.errMu.Lock()
		if len(c.serverErrors) < 100 {
			c.serverErrors = append(c.serverErrors, line)
		}
		c.errMu.Unlock()
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, bufio.ErrTooLong) {
		c.recordError(fmt.Sprintf("stderr reader failed: %v", err))
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
// whose read loop has ended is dead regardless of whether anyone called Close:
// the Manager uses this to replace a crashed server instead of handing out a
// corpse to every later call.
func (c *Client) Alive() bool {
	select {
	case <-c.doneCh:
		return false
	default:
		return true
	}
}

// Done exposes the channel closed when the server's stdout ends.
func (c *Client) Done() <-chan struct{} { return c.doneCh }

// readLimitedLine returns one newline-terminated frame without its terminator.
// The final unterminated frame at EOF counts as a line, matching the
// bufio.Scanner semantics this replaced. A frame longer than limit is drained
// to its terminator and reported as errLineTooLarge rather than ending the
// stream: one runaway frame must not convert a healthy session into permanent
// tool failure.
//
// The prefix is kept because whether an over-cap frame was a JSON-RPC frame or
// server chatter decides who has to hear about it. `xxxxx…` on stdout is a
// noisy server and the session carries on; a 17 MiB line that opens with `{`
// was a reply, and the request it was a reply to must be failed rather than
// left to sit out its full timeout on a response that is never coming.
type errLineTooLarge struct{ prefix []byte }

func (errLineTooLarge) Error() string {
	return fmt.Sprintf("server emitted a frame exceeding %d bytes", maxStdoutLine)
}

func readLimitedLine(r *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		switch {
		case err == nil:
			return append(buf, chunk...), nil
		case err == bufio.ErrBufferFull:
			buf = append(buf, chunk...)
			if len(buf) > limit {
				drainToNewline(r)
				prefix := buf
				if len(prefix) > unroutablePreview {
					prefix = prefix[:unroutablePreview]
				}
				return nil, errLineTooLarge{prefix: append([]byte(nil), prefix...)}
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
		if err == nil || (err != nil && err != bufio.ErrBufferFull) {
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
		case err == io.EOF:
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
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
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

	var envelope struct {
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	// The strict decode above already succeeded, so this one cannot fail; the
	// error is checked anyway rather than discarded.
	if err := json.Unmarshal(line, &envelope); err != nil {
		c.recordError("ignored unreadable frame: " + truncateForMessage(string(line)))
		return
	}
	if envelope.Method != "" {
		return
	}
	if len(envelope.Result) == 0 && len(envelope.Error) == 0 && resp.JSONRPC == "" {
		// A JSON object that is not claiming to be a JSON-RPC reply — a server
		// logging structured lines to stdout. Not ours to fail calls over.
		c.recordError("ignored non-JSON-RPC object on stdout: " + truncateForMessage(string(line)))
		return
	}

	id, ok := numericID(resp.ID)
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
			JSONRPC: JSONRPCVersion,
			Error:   &RPCError{Code: CodeInternalError, Message: reason},
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
	if c.closed.Load() {
		return errors.New("mcp: client is closed")
	}
	if !c.Alive() {
		return fmt.Errorf("mcp: server %s has exited%s", c.cfg.Name, c.diagnosticsSuffix())
	}
	if _, ok := ctx.Deadline(); !ok {
		ceiling := c.cfg.Timeout
		if ceiling <= 0 {
			ceiling = defaultCallTimeout
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, ceiling)
		defer cancel()
	}

	id := c.reqCounter.Add(1)

	var paramsRaw json.RawMessage
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return fmt.Errorf("mcp: encoding params for %s: %w", method, err)
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
		return fmt.Errorf("mcp: encoding request for %s: %w", method, err)
	}
	data = append(data, '\n')

	respCh := make(chan *Response, 1)
	c.pendingMu.Lock()
	c.pending[id] = respCh
	c.pendingMu.Unlock()

	if writeErr := c.writeFrame(data); writeErr != nil {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		return fmt.Errorf("mcp: writing request %s to %s: %w", method, c.cfg.Name, writeErr)
	}

	select {
	case <-ctx.Done():
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
		// Whatever the server said about itself travels with the timeout. A
		// wedge whose cause was recorded on stderr and then discarded is how
		// "the tool call hung" became the whole of an operator's evidence.
		return fmt.Errorf("mcp: %s on %s: %w%s", method, c.cfg.Name, ctx.Err(), c.diagnosticsSuffix())
	case <-c.doneCh:
		return fmt.Errorf("mcp: server %s exited unexpectedly%s", c.cfg.Name, c.diagnosticsSuffix())
	case resp, ok := <-respCh:
		if !ok || resp == nil {
			return fmt.Errorf("mcp: server %s closed connection before replying to %s%s",
				c.cfg.Name, method, c.diagnosticsSuffix())
		}
		if resp.Error != nil {
			return fmt.Errorf("mcp: %s returned error: %s (code %d)", c.cfg.Name, resp.Error.Message, resp.Error.Code)
		}
		if result != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("mcp: unmarshaling %s result from %s: %w", method, c.cfg.Name, err)
			}
		}
		return nil
	}
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
		if c.stdin != nil {
			// Safe to call while that write is still in flight: StdinPipe
			// returns a close-once handle over a pollable pipe, so the
			// blocked Write returns instead of holding its buffer forever.
			_ = c.stdin.Close()
		}
		return fmt.Errorf("stdin write to %s did not complete within %s (server not reading); "+
			"the connection was retired because the frame may be partly written",
			c.cfg.Name, writeTimeout)
	}
}

// Notify sends a one-way notification without waiting for a response.
func (c *Client) Notify(method string, params any) error {
	if c.closed.Load() {
		return errors.New("mcp: client is closed")
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

// Initialize performs the MCP handshake.
//
// A failed attempt does not poison the client: the handshake is retried on the
// next call. The sync.Once this replaced kept the first error forever — a
// single cancelled context during startup would have left every later call
// failing with a stale failure even after the server came up.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	c.initMu.Lock()
	defer c.initMu.Unlock()
	if c.initTried && c.initErr == nil {
		return c.initResult, nil
	}

	params := InitializeParams{
		ProtocolVersion: ProtocolVersionLatest,
		Capabilities: ClientCapabilities{
			Roots: &RootsCapability{ListChanged: true},
		},
		ClientInfo: Implementation{
			Name:    "manvi",
			Version: "1.0.0",
		},
	}

	var res InitializeResult
	if err := c.Call(ctx, "initialize", params, &res); err != nil {
		c.initErr = err
		c.initTried = false // a later call may retry; the server may recover
		return nil, err
	}
	c.initResult = &res
	c.initErr = nil
	c.initTried = true

	// Send initialized notification
	if err := c.Notify("notifications/initialized", map[string]any{}); err != nil {
		c.initErr = err
		c.initTried = false
		return nil, fmt.Errorf("mcp: notifying initialization to %s: %w", c.cfg.Name, err)
	}

	return c.initResult, nil
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
			killProcessGroup(c.cmd.Process.Pid)
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
