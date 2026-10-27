package computer

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// slowBroker is a stand-in broker that answers each request, echoing its id,
// after delay seconds.
func slowBroker(t *testing.T, delay string) *Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in broker is a shell script")
	}
	script := "#!/bin/sh\nwhile read line; do\n id=$(printf '%s' \"$line\" | sed -E 's/.*\"id\":\"([^\"]*)\".*/\\1/')\n sleep " + delay +
		"\n printf '{\"id\":\"%s\",\"ok\":true,\"result\":{\"answered\":\"%s\"}}\\n' \"$id\" \"$id\"\ndone\n"
	bin := filepath.Join(t.TempDir(), "manvi-desktop")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a stand-in executable under t.TempDir must be executable
		t.Fatal(err)
	}
	c, err := Start(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// A read that outlives the runner's per-operation deadline must cost that one
// call, not the run: nothing in the repository starts a second broker, so a
// retired one leaves computer use dead until the process restarts.
func TestSlowReadOnlyCallDoesNotRetireTheBroker(t *testing.T) {
	for _, op := range []string{"observe", "resolve", "probe"} {
		t.Run(op, func(t *testing.T) {
			c := slowBroker(t, "0.4")
			slow, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if _, err := c.Call(slow, op, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("the slow call err = %v, want the deadline", err)
			}
			next, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			raw, err := c.Call(next, op, nil, nil)
			if err != nil {
				t.Fatalf("a call after a slow one failed: %v", err)
			}
			// The first request's late reply was discarded; this one is its own.
			var got struct{ Answered string }
			if err := json.Unmarshal(raw, &got); err != nil || got.Answered != "request-2" {
				t.Fatalf("the second call was answered with %s (err %v), want request-2's own reply", raw, err)
			}
		})
	}
}

// Every other op may act, or lift a fence, or move focus, so a cancel after its
// request is written retires the broker. An op nobody listed fails closed.
func TestCancelAfterWriteRetiresTheBrokerExceptForReadOnlyOps(t *testing.T) {
	for op, readOnly := range map[string]bool{
		"probe": true, "observe": true, "resolve": true, "pause": true, "detach": true,
		"act": false, "human_act": false, "approve": false, "attach": false, "focus": false, "resume": false,
		"an-op-added-later": false,
	} {
		t.Run(op, func(t *testing.T) {
			reqR, reqW := io.Pipe()
			respR, respW := io.Pipe()
			c := &Client{cmd: &exec.Cmd{}, in: reqW, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: make(chan struct{})}
			go c.read(respR)
			t.Cleanup(func() { _ = respW.Close(); _ = reqR.Close() })
			got := make(chan struct{}, 1)
			go func() {
				sc := bufio.NewScanner(reqR)
				for sc.Scan() {
					select {
					case got <- struct{}{}:
					default:
					}
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			returned := make(chan error, 1)
			go func() { _, err := c.Call(ctx, op, nil, nil); returned <- err }()
			<-got
			// The write goroutine frees the slot after it has reported the finished
			// write, so an empty slot means Call can see the write is done.
			for len(c.write) != 0 {
				time.Sleep(time.Millisecond)
			}
			cancel()
			if err := <-returned; !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want context.Canceled", err)
			}
			c.mu.Lock()
			retired := c.closed
			c.mu.Unlock()
			if retired == readOnly {
				t.Fatalf("broker retired = %v after a cancelled %s, want %v", retired, op, !readOnly)
			}
		})
	}
}

// A reply that is in hand as the context ends is a completed call: the caller
// gets the receipt, not a cancellation, and the broker is left alone. select
// picks at random between a ready reply and a done context, so the state is
// built directly: both are ready every time.
func TestReplyInHandWinsOverCancellation(t *testing.T) {
	for _, op := range []string{"act", "observe"} {
		for range 50 {
			c := &Client{cmd: &exec.Cmd{}, in: nopWriteCloser{}, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: make(chan struct{})}
			ch := make(chan pendingResult, 1)
			ch <- pendingResult{response: response{ID: "request-1", OK: true, Result: json.RawMessage(`{"done":true}`)}}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			raw, err := c.awaitReply(ctx, op, ch)
			if err != nil || string(raw) != `{"done":true}` {
				t.Fatalf("%s: a call whose reply had arrived returned %s, %v", op, raw, err)
			}
			c.mu.Lock()
			retired := c.closed
			c.mu.Unlock()
			if retired {
				t.Fatalf("%s: the broker was retired though the call completed", op)
			}
		}
	}
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }
