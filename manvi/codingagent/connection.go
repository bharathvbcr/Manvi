package codingagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/internal/proc"
)

type packet struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}
type wireRead struct {
	value packet
	err   error
}

// Exit proves the direct child was reaped. Descendant termination or a successful
// coding result requires separate host evidence; an exit code alone proves neither.
type Exit struct {
	Reaped bool
	Code   int
}
type connection struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	pid       int
	sequence  uint64
	frames    chan wireRead
	done      chan struct{}
	waitErr   error
	closeOnce sync.Once
}
type limitedDiscard struct {
	remaining int
	cancel    context.CancelFunc
}

func (w *limitedDiscard) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		w.cancel()
		return 0, errors.New("managed provider stderr exceeded 64 KiB")
	}
	w.remaining -= len(p)
	return len(p), nil
}

func startConnection(parent context.Context, program string, args []string, cwd string) (*connection, error) {
	if err := parent.Err(); err != nil {
		return nil, beforeStart(err)
	}
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, program, args...)
	cmd.Dir = cwd
	cmd.WaitDelay = 2 * time.Second
	proc.ConfigureGroup(cmd)
	// Preserve authentication and ordinary provider preferences. Remove only Git
	// repository-routing overrides and replace the inherited Manvi root for this child.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_COUNT", "DEVCOUNCIL_ROOT", "PWD":
			continue
		}
		if strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_") {
			continue
		}
		cmd.Env = append(cmd.Env, entry)
	}
	cmd.Env = append(cmd.Env, "DEVCOUNCIL_ROOT="+cwd, "PWD="+cwd)
	cmd.Stderr = &limitedDiscard{remaining: 65536, cancel: cancel}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, beforeStart(err)
	}
	// Cmd.Wait must not close a StdoutPipe ahead of its reader. Let os/exec
	// finish its output copy before closing our owned stream on process exit.
	stdout, output := io.Pipe()
	cmd.Stdout = output
	if err = cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		stdout.Close()
		output.Close()
		return nil, beforeStart(fmt.Errorf("start managed provider: %w", err))
	}
	c := &connection{ctx: ctx, cancel: cancel, cmd: cmd, stdin: stdin, stdout: stdout, pid: cmd.Process.Pid, frames: make(chan wireRead, 64), done: make(chan struct{})}
	go c.read()
	go func() { c.waitErr = cmd.Wait(); output.Close(); close(c.done) }()
	return c, nil
}
func (c *connection) read() {
	defer close(c.frames)
	defer c.stdout.Close()
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	total := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		total += len(line)
		var p packet
		err := validatePacket(line)
		if total > 32*1024*1024 {
			err = errors.New("managed provider output exceeded 32 MiB")
		}
		if err == nil {
			err = json.Unmarshal(line, &p)
		}
		select {
		case c.frames <- wireRead{p, err}:
		case <-c.ctx.Done():
			return
		}
		if err != nil {
			c.cancel()
			return
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case c.frames <- wireRead{err: err}:
	case <-c.ctx.Done():
	}
}
func validatePacket(raw []byte) error {
	if len(raw) > MaxFrameBytes || !utf8.Valid(raw) {
		return errors.New("invalid or oversized Codex protocol frame")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return errors.New("Codex protocol frame must be an object")
	}
	seen := make(map[string]bool)
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return err
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return errors.New("Codex protocol has a duplicate field")
		}
		seen[key] = true
		var value json.RawMessage
		if err = d.Decode(&value); err != nil {
			return err
		}
	}
	if _, err = d.Token(); err != nil {
		return err
	}
	if _, err = d.Token(); err != io.EOF {
		return errors.New("Codex protocol frame has trailing content")
	}
	if !seen["method"] && (!seen["id"] || seen["result"] == seen["error"]) {
		return errors.New("Codex frame is neither an event nor a response")
	}
	if seen["method"] && (seen["result"] || seen["error"]) {
		return errors.New("Codex frame mixes a request and response")
	}
	return nil
}
func (c *connection) nextID() json.RawMessage {
	c.sequence++
	return []byte(strconv.FormatUint(c.sequence, 10))
}
func (c *connection) send(ctx context.Context, p packet) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(data) > MaxFrameBytes {
		return errors.New("managed request exceeds its wire budget")
	}
	done := make(chan error, 1)
	go func() { _, err := c.stdin.Write(append(data, '\n')); done <- err }()
	select {
	case err = <-done:
		if err != nil {
			c.cancel()
		}
		return err
	case <-ctx.Done():
		c.cancel()
		return ctx.Err()
	case <-c.ctx.Done():
		return c.ctx.Err()
	}
}
func (c *connection) receive(ctx context.Context) (packet, error) {
	// A timeout observing a live session does not restart or terminate it.
	select {
	case next, ok := <-c.frames:
		if !ok {
			return packet{}, io.EOF
		}
		return next.value, next.err
	case <-ctx.Done():
		return packet{}, ctx.Err()
	case <-c.ctx.Done():
		return packet{}, c.ctx.Err()
	}
}
func (c *connection) close() (Exit, error) {
	c.closeOnce.Do(func() { c.stdin.Close(); c.cancel(); c.stdout.Close() })
	select {
	case <-c.done:
		result := Exit{Reaped: true, Code: c.cmd.ProcessState.ExitCode()}
		var exit *exec.ExitError
		if c.waitErr != nil && !errors.As(c.waitErr, &exit) && !errors.Is(c.waitErr, context.Canceled) {
			return result, c.waitErr
		}
		return result, nil
	case <-time.After(5 * time.Second):
		return Exit{}, errors.New("managed provider did not confirm process reaping before shutdown deadline")
	}
}
