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
	"strings"
	"testing"
	"time"
)

// actionBroker starts a stand-in desktop broker, a shell script, that stands for
// a broker which takes a request and acts on it a moment later: it records that
// the request arrived, waits, and then records that the action ran. The action
// file is what a click, a keystroke or a typed secret would be.
func actionBroker(t *testing.T, readFirst bool) (c *Client, received, action string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in broker is a shell script")
	}
	dir := t.TempDir()
	received = filepath.Join(dir, "received")
	action = filepath.Join(dir, "action")
	script := "#!/bin/sh\n"
	if readFirst {
		script += "read line\n"
	}
	script += ": > '" + received + "'\nsleep 1\n: > '" + action + "'\n"
	bin := filepath.Join(dir, "manvi-desktop")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil { // #nosec G306 -- a stand-in executable under t.TempDir must be executable
		t.Fatal(err)
	}
	c, err := Start(context.Background(), bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, received, action
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The broker is retired when it exits; only then is it certain that no action
// can still be dispatched, so the checks below read the outcome after that.
func brokerGone(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.done:
	case <-time.After(safetyTimeout):
		t.Fatal("the broker is still running after the call was cancelled")
	}
}

func writeSlotFree(t *testing.T, c *Client) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(c.write) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the write goroutine of a cancelled call never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Cancelling after the broker has the request must retire the broker, or the
// action runs after the caller has given up.
func TestCancelAfterTheRequestIsWrittenStopsTheBroker(t *testing.T) {
	c, received, action := actionBroker(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "act", nil, nil); returned <- err }()
	waitForFile(t, received)
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(safetyTimeout):
		t.Fatal("a cancelled call did not return")
	}
	brokerGone(t, c)
	if _, err := os.Stat(action); err == nil {
		t.Fatal("the action ran after the call was cancelled")
	}
	writeSlotFree(t, c)
	if _, err := c.Call(context.Background(), "act", nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a call on the retired client err = %v, want the cancellation that retired it", err)
	}
}

// Cancelling while the request is still being written already stopped the
// broker; the post-write fix must not lose that. A request larger than the pipe
// buffer, to a broker that never reads, keeps the write blocked.
func TestCancelWhileTheRequestIsBeingWrittenStopsTheBroker(t *testing.T) {
	c, _, action := actionBroker(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	big := json.RawMessage(`"` + strings.Repeat("x", 200<<10) + `"`)
	go func() { _, err := c.Call(ctx, "act", nil, big); returned <- err }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-returned:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(safetyTimeout):
		t.Fatal("a cancelled write did not return")
	}
	brokerGone(t, c)
	if _, err := os.Stat(action); err == nil {
		t.Fatal("the action ran after the call was cancelled")
	}
	writeSlotFree(t, c)
}

// A reply that arrives after the call was cancelled belongs to nobody: the
// client is retired, so it cannot satisfy a newer call either.
func TestLateReplyAfterCancelIsNotAppliedToANewerCall(t *testing.T) {
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	c := &Client{cmd: &exec.Cmd{}, in: reqW, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: make(chan struct{})}
	go c.read(respR)
	t.Cleanup(func() { _ = respW.Close(); _ = reqR.Close() })
	got := make(chan request, 1)
	go func() {
		sc := bufio.NewScanner(reqR)
		for n := 0; sc.Scan(); n++ {
			var req request
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				return
			}
			if n == 0 {
				// The first request is held: its reply is sent late, by the test.
				got <- req
				continue
			}
			// Every later request is answered at once, as a healthy broker would.
			out, _ := json.Marshal(response{ID: req.ID, OK: true, Result: json.RawMessage(`{"newer":true}`)})
			if _, err := respW.Write(append(out, '\n')); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "act", nil, nil); returned <- err }()
	firstReq := <-got
	cancel()
	if err := <-returned; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}

	// The broker answers the cancelled request late, and then a second one.
	late, _ := json.Marshal(response{ID: firstReq.ID, OK: true, Result: json.RawMessage(`{"late":true}`)})
	go func() { _, _ = respW.Write(append(late, '\n')) }()
	newer, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	raw, err := c.Call(newer, "observe", nil, nil)
	if err == nil {
		t.Fatalf("a newer call on a client whose call was cancelled succeeded with %s", raw)
	}
	if strings.Contains(string(raw), "late") {
		t.Fatalf("the late reply was applied to a newer call: %s", raw)
	}
}
