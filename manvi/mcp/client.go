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

	initOnce     sync.Once
	initResult   *InitializeResult
	initErr      error
	closed       atomic.Bool
	doneCh       chan struct{}
	serverErrors []string
	errMu        sync.Mutex
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
	for scanner.Scan() {
		line := scanner.Text()
		c.errMu.Lock()
		if len(c.serverErrors) < 100 {
			c.serverErrors = append(c.serverErrors, line)
		}
		c.errMu.Unlock()
	}
}

// readLoop processes incoming JSON-RPC lines from the server.
func (c *Client) readLoop() {
	defer close(c.doneCh)
	scanner := bufio.NewScanner(c.stdout)
	// Allow large tool output buffers (up to 16MB)
	buf := make([]byte, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp Response
		if err := json.Unmarshal(line, &resp); err != nil {
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
func (c *Client) Call(ctx context.Context, method string, params any, result any) error {
	if c.closed.Load() {
		return errors.New("mcp: client is closed")
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

	c.writerMu.Lock()
	_, writeErr := c.stdin.Write(data)
	c.writerMu.Unlock()

	if writeErr != nil {
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

// Notify sends a one-way notification without waiting for a response.
func (c *Client) Notify(method string, params any) error {
	if c.closed.Load() {
		return errors.New("mcp: client is closed")
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

	c.writerMu.Lock()
	defer c.writerMu.Unlock()
	_, writeErr := c.stdin.Write(data)
	return writeErr
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) (*InitializeResult, error) {
	c.initOnce.Do(func() {
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
			return
		}
		c.initResult = &res

		// Send initialized notification
		_ = c.Notify("notifications/initialized", map[string]any{})
	})

	return c.initResult, c.initErr
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
