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
	"sync"
	"sync/atomic"
	"time"
)

// ServerConfig configures an MCP server process.
type ServerConfig struct {
	Name    string            `json:"name,omitempty"`
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Timeout time.Duration     `json:"timeout,omitempty"`
}

// Client is a JSON-RPC 2.0 client connected to an MCP server process over stdio.
type Client struct {
	cfg        ServerConfig
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	writerMu   sync.Mutex
	reqCounter atomic.Int64

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
)

// NewClient spawns a new MCP server subprocess and connects stdio pipes.
func NewClient(cfg ServerConfig) (*Client, error) {
	if cfg.Command == "" {
		return nil, errors.New("mcp: server command is required")
	}

	cmd := exec.Command(cfg.Command, cfg.Args...)
	if cfg.Cwd != "" {
		cmd.Dir = cfg.Cwd
	}

	// Environment inheritance + custom variables
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

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
type errLineTooLarge struct{}

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
				return nil, errLineTooLarge{}
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
		switch {
		case err == nil:
		case errors.As(err, new(errLineTooLarge)):
			c.recordError(err.Error())
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

		var resp Response
		if json.Unmarshal(line, &resp) != nil {
			continue
		}

		// Match by numeric ID
		var id int64
		switch v := resp.ID.(type) {
		case float64:
			id = int64(v)
		case int64:
			id = v
		case int:
			id = int64(v)
		default:
			// Notification or non-numeric ID; ignore for now
			continue
		}

		c.pendingMu.Lock()
		ch, ok := c.pending[id]
		if ok {
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()

		if ok && ch != nil {
			ch <- &resp
		}
	}

	// Flush all remaining pending requests on EOF
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.pendingMu.Unlock()
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
		return fmt.Errorf("mcp: server %s has exited", c.cfg.Name)
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
		return ctx.Err()
	case <-c.doneCh:
		return fmt.Errorf("mcp: server %s exited unexpectedly", c.cfg.Name)
	case resp, ok := <-respCh:
		if !ok || resp == nil {
			return fmt.Errorf("mcp: server %s closed connection before replying to %s", c.cfg.Name, method)
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
// The goroutine a timed-out write leaves behind drains on its own; the pipe is
// closed by Close either way.
func (c *Client) writeFrame(data []byte) error {
	c.writerMu.Lock()
	defer c.writerMu.Unlock()

	errCh := make(chan error, 1)
	go func() {
		_, werr := c.stdin.Write(data)
		errCh <- werr
	}()

	select {
	case werr := <-errCh:
		return werr
	case <-time.After(writeTimeout):
		return fmt.Errorf("stdin write did not complete within %s (server not reading)", writeTimeout)
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

// ListTools returns all tools offered by the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	if _, err := c.Initialize(ctx); err != nil {
		return nil, err
	}

	var res ListToolsResult
	if err := c.Call(ctx, "tools/list", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Tools, nil
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
