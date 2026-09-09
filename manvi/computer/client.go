// Package computer is the bounded client for the native desktop broker.
package computer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bharathvbcr/Manvi/manvi/internal/proc"
	"github.com/bharathvbcr/Manvi/manvi/workflow"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

const maxResponseBytes = 20 << 20
const maxRequestBytes = 256 << 10

type Bounds struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}
type Window struct {
	PID             uint32  `json:"pid"`
	ID              uint64  `json:"window_id"`
	Title           string  `json:"title"`
	ProcessIdentity string  `json:"process_identity"`
	Bounds          Bounds  `json:"bounds"`
	Foreground      bool    `json:"foreground"`
	Scale           float64 `json:"scale"`
}
type Node struct {
	ID         string   `json:"id"`
	Role       string   `json:"role"`
	Name       string   `json:"name"`
	Value      *string  `json:"value,omitempty"`
	Identifier *string  `json:"identifier,omitempty"`
	Parent     *string  `json:"parent,omitempty"`
	Bounds     *Bounds  `json:"bounds,omitempty"`
	Enabled    bool     `json:"enabled"`
	Editable   bool     `json:"editable"`
	Actions    []string `json:"actions"`
	NativePath []uint32 `json:"native_path"`
}
type Screenshot struct {
	MIMEType string `json:"mime_type"`
	Base64   string `json:"base64"`
	Width    uint32 `json:"width"`
	Height   uint32 `json:"height"`
}
type Observation struct {
	ID               string     `json:"observation_id"`
	Epoch            uint64     `json:"epoch"`
	CapturedAtMillis uint64     `json:"captured_at_ms"`
	TreeAtMillis     uint64     `json:"tree_at_ms"`
	Window           Window     `json:"window"`
	Nodes            []Node     `json:"nodes"`
	Screenshot       Screenshot `json:"screenshot"`
	Complete         bool       `json:"complete"`
	TruncatedReason  string     `json:"truncated_reason,omitempty"`
}
type Selector struct {
	Visual       *workflow.VisualAnchor `json:"visual,omitempty"`
	Role         string                 `json:"role,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Identifier   string                 `json:"identifier,omitempty"`
	AncestorName string                 `json:"ancestor_name,omitempty"`
}
type Session struct {
	ID     string `json:"session_id"`
	RunID  string `json:"run_id"`
	Epoch  uint64 `json:"epoch"`
	Window Window `json:"window"`
}
type Action struct {
	ActionID      string   `json:"action_id"`
	ObservationID string   `json:"observation_id"`
	TargetID      string   `json:"target_id"`
	Kind          string   `json:"kind"`
	Text          *string  `json:"text,omitempty"`
	X             *float64 `json:"x,omitempty"`
	Y             *float64 `json:"y,omitempty"`
	ScrollY       *int32   `json:"scroll_y,omitempty"`
	Effect        string   `json:"effect"`
	ApprovalID    string   `json:"approval_id,omitempty"`
}
type Receipt struct {
	ActionID           string `json:"action_id"`
	Delivery           string `json:"delivery"`
	DispatchedAtMillis uint64 `json:"dispatched_at_ms"`
	Verified           bool   `json:"verified"`
}
type BrokerError struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Delivery string `json:"delivery"`
}

func (e *BrokerError) Error() string { return e.Code + ": " + e.Message }

type request struct {
	ID        string          `json:"id"`
	Op        string          `json:"op"`
	SessionID string          `json:"session_id,omitempty"`
	RunID     string          `json:"run_id,omitempty"`
	Epoch     uint64          `json:"epoch,omitempty"`
	Payload   json.RawMessage `json:"payload"`
}
type response struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *BrokerError    `json:"error"`
}
type pendingResult struct {
	response response
	err      error
}
type Client struct {
	cmd      *exec.Cmd
	in       io.WriteCloser
	write    chan struct{}
	stopOnce sync.Once
	stopErr  error
	mu       sync.Mutex
	pending  map[string]chan pendingResult
	closed   bool
	terminal error
	next     atomic.Uint64
	done     chan struct{}
	waitErr  error
}

func Start(ctx context.Context, binary string) (*Client, error) {
	if binary == "" {
		return nil, errors.New("native broker binary required")
	}
	cmd := exec.CommandContext(ctx, binary, "serve")
	proc.ConfigureGroup(cmd)
	cmd.WaitDelay = 2 * time.Second
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return nil, err
	}
	// Child stderr is deliberately not forwarded to evidence or model logs.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		in.Close()
		return nil, err
	}
	c := &Client{cmd: cmd, in: in, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: make(chan struct{})}
	go c.read(out)
	return c, nil
}
func (c *Client) read(r io.Reader) {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64<<10), maxResponseBytes)
	var terminal error
	for s.Scan() {
		var reply response
		if err := json.Unmarshal(s.Bytes(), &reply); err != nil {
			terminal = fmt.Errorf("invalid broker response: %w", err)
			break
		}
		c.mu.Lock()
		ch := c.pending[reply.ID]
		delete(c.pending, reply.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- pendingResult{response: reply}
		}
	}
	if terminal == nil {
		terminal = s.Err()
	}
	if terminal == nil {
		terminal = io.EOF
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.terminal = terminal
	}
	pending := c.pending
	c.pending = map[string]chan pendingResult{}
	c.mu.Unlock()
	for _, ch := range pending {
		ch <- pendingResult{err: terminal}
	}
	if c.cmd.Process != nil && terminal != io.EOF {
		proc.KillGroup(c.cmd.Process.Pid)
		_ = c.cmd.Process.Kill()
	}
	err := c.cmd.Wait()
	c.mu.Lock()
	c.waitErr = err
	c.mu.Unlock()
	close(c.done)
}

// stop breaks a blocked pipe write without waiting for its serialization lock.
// Any partially sent request is uncertain; the process is retired, never reused.
func (c *Client) stop(cause error) error {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.closed = true
		c.terminal = cause
		pending := c.pending
		c.pending = map[string]chan pendingResult{}
		c.mu.Unlock()
		for _, ch := range pending {
			ch <- pendingResult{err: cause}
		}
		var killErr error
		if c.cmd != nil && c.cmd.Process != nil {
			proc.KillGroup(c.cmd.Process.Pid)
			killErr = c.cmd.Process.Kill()
			if errors.Is(killErr, os.ErrProcessDone) {
				killErr = nil
			}
		}
		closeErr := c.in.Close()
		if errors.Is(closeErr, os.ErrClosed) {
			closeErr = nil
		}
		c.stopErr = errors.Join(killErr, closeErr)
	})
	return c.stopErr
}
func (c *Client) Close() error {
	err := c.stop(io.EOF)
	<-c.done
	return err
}

func (c *Client) Call(ctx context.Context, op string, session *Session, payload json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("request-%d", c.next.Add(1))
	req := request{ID: id, Op: op, Payload: payload}
	if len(payload) == 0 {
		req.Payload = json.RawMessage(`{}`)
	}
	if session != nil {
		req.SessionID = session.ID
		req.RunID = session.RunID
		req.Epoch = session.Epoch
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxRequestBytes {
		return nil, errors.New("native request exceeds 256 KiB")
	}
	ch := make(chan pendingResult, 1)
	c.mu.Lock()
	if c.closed {
		err := c.terminal
		c.mu.Unlock()
		return nil, err
	}
	if len(c.pending) >= 64 {
		c.mu.Unlock()
		return nil, errors.New("native request queue full")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case c.write <- struct{}{}:
	}
	written := make(chan error, 1)
	go func() {
		defer func() { <-c.write }()
		if err := ctx.Err(); err != nil {
			written <- err
			return
		}
		packet := append(raw, '\n')
		n, err := c.in.Write(packet)
		if err == nil && n != len(packet) {
			err = io.ErrShortWrite
		}
		written <- err
	}()
	select {
	case <-ctx.Done():
		return nil, errors.Join(ctx.Err(), c.stop(ctx.Err()))
	case err := <-written:
		if err != nil {
			return nil, errors.Join(err, c.stop(err))
		}
	}
	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case result := <-ch:
		if result.err != nil {
			return nil, result.err
		}
		if !result.response.OK {
			if len(result.response.Result) != 0 && string(result.response.Result) != "null" {
				return nil, errors.New("inconsistent broker failure response")
			}
			if result.response.Error == nil {
				return nil, errors.New("broker rejected request without structured reason")
			}
			return nil, result.response.Error
		}
		if result.response.Error != nil || len(result.response.Result) == 0 {
			return nil, errors.New("inconsistent broker success response")
		}
		return result.response.Result, nil
	}
}

func (c *Client) Probe(ctx context.Context) (json.RawMessage, error) {
	return c.Call(ctx, "probe", nil, nil)
}
func (c *Client) Attach(ctx context.Context, runID string, pid uint32, readOnly []Selector) (Session, error) {
	return c.attach(ctx, runID, pid, readOnly, false)
}

// AttachFocused explicitly authorizes focusing the selected application's window
// before attachment. This avoids capturing an unfocused Stage Manager thumbnail.
func (c *Client) AttachFocused(ctx context.Context, runID string, pid uint32, readOnly []Selector) (Session, error) {
	return c.attach(ctx, runID, pid, readOnly, true)
}

func (c *Client) attach(ctx context.Context, runID string, pid uint32, readOnly []Selector, focus bool) (Session, error) {
	if runID == "" || pid == 0 {
		return Session{}, errors.New("run identity and target PID required")
	}
	if readOnly == nil {
		readOnly = []Selector{}
	}
	payload, err := json.Marshal(struct {
		PID      uint32     `json:"pid"`
		ReadOnly []Selector `json:"read_only_targets"`
		Focus    bool       `json:"focus"`
	}{pid, readOnly, focus})
	if err != nil {
		return Session{}, err
	}
	s := Session{RunID: runID}
	raw, err := c.Call(ctx, "attach", &s, payload)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, err
	}
	s.RunID = runID
	if s.ID == "" || s.Epoch == 0 || s.Window.PID != pid {
		return s, errors.New("invalid native attachment identity")
	}
	return s, nil
}

func (c *Client) Focus(ctx context.Context, s Session) (Session, error) {
	raw, err := c.Call(ctx, "focus", &s, nil)
	if err != nil {
		return s, err
	}
	var reply struct {
		Window Window `json:"window"`
		Epoch  uint64 `json:"epoch"`
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return s, err
	}
	if reply.Epoch != s.Epoch || reply.Window.ID != s.Window.ID || reply.Window.PID != s.Window.PID || reply.Window.ProcessIdentity != s.Window.ProcessIdentity {
		return s, errors.New("focus changed attached session identity")
	}
	s.Window = reply.Window
	return s, nil
}
func (c *Client) Observe(ctx context.Context, s Session) (Observation, error) {
	var o Observation
	raw, err := c.Call(ctx, "observe", &s, nil)
	if err != nil {
		return o, err
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, err
	}
	if o.ID == "" || o.Epoch != s.Epoch || o.Window.ProcessIdentity != s.Window.ProcessIdentity || o.Window.ID != s.Window.ID {
		return o, errors.New("native observation scope changed")
	}
	return o, nil
}
func (c *Client) Resolve(ctx context.Context, s Session, observationID string, selector Selector) (Node, error) {
	var reply struct {
		TargetID string `json:"target_id"`
		Node     Node   `json:"node"`
	}
	p, err := json.Marshal(struct {
		ObservationID string   `json:"observation_id"`
		Selector      Selector `json:"selector"`
	}{observationID, selector})
	if err != nil {
		return Node{}, err
	}
	raw, err := c.Call(ctx, "resolve", &s, p)
	if err != nil {
		return Node{}, err
	}
	if err := json.Unmarshal(raw, &reply); err != nil {
		return Node{}, err
	}
	if reply.TargetID == "" || reply.Node.ID != reply.TargetID {
		return Node{}, errors.New("invalid resolved target")
	}
	return reply.Node, nil
}
func (c *Client) Act(ctx context.Context, s Session, a Action) (Receipt, error) {
	var receipt Receipt
	p, err := json.Marshal(a)
	if err != nil {
		return receipt, err
	}
	raw, err := c.Call(ctx, "act", &s, p)
	if err != nil {
		return receipt, err
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ActionID != a.ActionID {
		return Receipt{}, errors.New("action receipt identity mismatch")
	}
	return receipt, nil
}
func (c *Client) Approve(ctx context.Context, s Session, a Action) (string, error) {
	p, err := json.Marshal(a)
	if err != nil {
		return "", err
	}
	raw, err := c.Call(ctx, "approve", &s, p)
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"approval_id"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", err
	}
	if result.ID == "" {
		return "", errors.New("approval id missing")
	}
	return result.ID, nil
}
func (c *Client) Control(ctx context.Context, s Session, op string) (Session, error) {
	if op != "pause" && op != "detach" {
		return s, errors.New("invalid control operation")
	}
	raw, err := c.Call(ctx, op, &s, nil)
	if err != nil {
		return s, err
	}
	if op == "detach" {
		return s, nil
	}
	var result struct {
		Epoch uint64 `json:"epoch"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return s, err
	}
	if result.Epoch < s.Epoch {
		return s, errors.New("broker epoch regressed")
	}
	s.Epoch = result.Epoch
	return s, nil
}

func (c *Client) Resume(ctx context.Context, s Session, observationID string) (Session, error) {
	p, err := json.Marshal(struct {
		ObservationID string `json:"observation_id"`
	}{observationID})
	if err != nil {
		return s, err
	}
	raw, err := c.Call(ctx, "resume", &s, p)
	if err != nil {
		return s, err
	}
	var result struct {
		Epoch uint64 `json:"epoch"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return s, err
	}
	if result.Epoch <= s.Epoch {
		return s, errors.New("resume did not advance control epoch")
	}
	s.Epoch = result.Epoch
	return s, nil
}

func (c *Client) HumanAct(ctx context.Context, s Session, a Action) (Receipt, error) {
	var receipt Receipt
	p, err := json.Marshal(a)
	if err != nil {
		return receipt, err
	}
	raw, err := c.Call(ctx, "human_act", &s, p)
	if err != nil {
		return receipt, err
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return receipt, err
	}
	if receipt.ActionID != a.ActionID {
		return Receipt{}, errors.New("human receipt identity mismatch")
	}
	return receipt, nil
}
