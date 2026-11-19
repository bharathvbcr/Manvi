package codingagent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Adversarial tests for the w23-managed fix of ft-50d0119f (connection end,
// close ordering, preferFailure). Prefix zadv_w23_.

func advW23Start(t *testing.T, script string, validate func([]byte) error) *connection {
	t.Helper()
	c, err := startConnection(t.Context(), "/bin/sh", []string{"-c", script}, t.TempDir(), validate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = c.close() })
	return c
}

func advW23Ask(i int) []byte {
	return []byte(fmt.Sprintf(`{"type":"control_request","request_id":"r%d","session_id":"S","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"ls"}}}`, i))
}

func advW23Claude(t *testing.T, c *connection, pending int) (*Claude, []Request) {
	t.Helper()
	s := &Claude{sessionID: "S", started: true, turnID: "T", pending: map[string]*claudePending{}, seen: map[string]bool{}, wire: c}
	var reqs []Request
	for i := 0; i < pending; i++ {
		e, ok, err := s.event(advW23Ask(i))
		if err != nil || !ok || e.Request == nil {
			t.Fatalf("capture %d: ok=%v err=%v", i, ok, err)
		}
		reqs = append(reqs, *e.Request)
	}
	return s, reqs
}

func advW23Pipeish(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed)
}

// Attack 2/3: after close() the connection reports the cancel. Not reachable
// from serve (the one caller of Next, managed.go:1556, runs Close itself after
// its loop, managed.go:1463; Stop only cancels the job context, managed.go:2167),
// so this is a contract gap, not a live misclassification. The reader's own
// read of the pipe close() shut (io.ErrClosedPipe) must never be what a later
// receive reports: serve classes the cancel "interrupted" and a read error
// "failed". The queued read-error frame and the done context are both ready,
// and the runtime picks at random.
func TestAdvW23ReceiveAfterCloseReportsTheCancelNotTheClosedPipe(t *testing.T) {
	bad := 0
	var last error
	for i := 0; i < 150; i++ {
		c := advW23Start(t, "exec cat >/dev/null", func([]byte) error { return nil })
		if _, err := c.close(); err != nil {
			t.Fatalf("run %d: close: %v", i, err)
		}
		ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := c.receive(ctx)
		stop()
		if !errors.Is(err, context.Canceled) || advW23Pipeish(err) {
			bad++
			last = err
		}
	}
	if bad > 0 {
		t.Fatalf("%d of 150 receives after close reported %v, want the cancel", bad, last)
	}
}

// The same with the reader already blocked in receive when close() lands, which
// is what a run interrupted mid-turn does.
func TestAdvW23BlockedReceiveWhenCloseLandsReportsTheCancel(t *testing.T) {
	bad := 0
	var last error
	for i := 0; i < 150; i++ {
		c := advW23Start(t, "exec cat >/dev/null", func([]byte) error { return nil })
		got := make(chan error, 1)
		go func() {
			ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			_, err := c.receive(ctx)
			got <- err
		}()
		time.Sleep(time.Millisecond)
		if _, err := c.close(); err != nil {
			t.Fatalf("run %d: close: %v", i, err)
		}
		if err := <-got; !errors.Is(err, context.Canceled) || advW23Pipeish(err) {
			bad++
			last = err
		}
	}
	if bad > 0 {
		t.Fatalf("%d of 150 blocked receives reported %v, want the cancel", bad, last)
	}
}

// Attack 7: Close, Next and Respond at once on a real process, under -race. The
// run ends without a hang, and every callback is withdrawn at most once.
func TestAdvW23ConcurrentCloseNextRespondOnARealProcess(t *testing.T) {
	for i := 0; i < 80; i++ {
		c := advW23Start(t, "exec cat >/dev/null", validateClaudeFrame)
		s, reqs := advW23Claude(t, c, 3)
		var wg sync.WaitGroup
		withdrawn := map[string]int{}
		var nextErr error
		wg.Add(3)
		go func() {
			defer wg.Done()
			for {
				ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
				e, err := s.Next(ctx)
				stop()
				if err != nil {
					nextErr = err
					return
				}
				if e.CancelledID != "" {
					withdrawn[e.CancelledID]++
				}
			}
		}()
		go func() {
			defer wg.Done()
			for _, r := range reqs {
				ctx, stop := context.WithTimeout(t.Context(), 5*time.Second)
				_ = s.ValidateResponse(r, Response{Decision: "allow_once"})
				_ = s.Respond(ctx, r, Response{Decision: "allow_once"})
				stop()
			}
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Duration(i%4) * 100 * time.Microsecond)
			_, _ = s.Close()
		}()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("run %d: Close, Next and Respond did not settle", i)
		}
		if nextErr == nil {
			t.Fatalf("run %d: Next never ended", i)
		}
		for id, n := range withdrawn {
			if n != 1 {
				t.Fatalf("run %d: %s withdrawn %d times", i, id, n)
			}
		}
	}
}

// Attack 2: an over-long line is a failure of the connection: the reader hands
// over the scanner's error, records it, and the agent is killed rather than left
// writing to a reader that has stopped.
func TestAdvW23OverlongLineFailsTheConnectionAndEndsTheAgent(t *testing.T) {
	c := advW23Start(t, "head -c 3000000 /dev/zero | tr '\\0' a; sleep 60", validateClaudeFrame)
	ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	_, err := c.receive(ctx)
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("receive = %v, want bufio.ErrTooLong", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for c.failure() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f := c.failure(); !errors.Is(f, bufio.ErrTooLong) {
		t.Fatalf("recorded failure = %v, want the scanner's error", f)
	}
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was left running after its over-long line")
	}
	if _, err := c.receive(ctx); !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("receive after the failure = %v, want the same cause", err)
	}
}

// An invalid frame is refused the same way (recorded, agent ended), and the
// recorded cause, not a bare EOF, is what the next read reports.
func TestAdvW23InvalidFrameFailsTheConnectionAndEndsTheAgent(t *testing.T) {
	c := advW23Start(t, "echo '{\"type\":'; sleep 60", validateClaudeFrame)
	ctx, stop := context.WithTimeout(t.Context(), 20*time.Second)
	defer stop()
	_, err := c.receive(ctx)
	if err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("receive = %v, want a refusal", err)
	}
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent was left running after an invalid frame")
	}
	if _, err2 := c.receive(ctx); err2 == nil || errors.Is(err2, io.EOF) || !strings.Contains(err2.Error(), err.Error()) && err2.Error() != err.Error() {
		t.Fatalf("receive after = %v, want the first cause %v", err2, err)
	}
}

// Attack 3: closing an agent that already left reports a clean exit with its own
// code and no error, and closing twice is the same answer.
func TestAdvW23CloseOfAnAgentThatAlreadyExitedIsClean(t *testing.T) {
	for i := 0; i < 40; i++ {
		c := advW23Start(t, "exit 0", func([]byte) error { return nil })
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			t.Fatal("agent did not exit")
		}
		for k := 0; k < 2; k++ {
			exit, err := c.close()
			if err != nil || !exit.Reaped || exit.Code != 0 || exit.GroupLive {
				t.Fatalf("run %d close %d: %+v, %v", i, k, exit, err)
			}
		}
	}
}

// Attack 3 (the other side of the written() change): a write that fails because
// the agent died on its own, with nobody cancelling, is still a failure of the
// pipe and not a cancel.
func TestAdvW23WriteToADeadAgentIsNotReportedAsACancel(t *testing.T) {
	for i := 0; i < 40; i++ {
		c := advW23Start(t, "exit 0", func([]byte) error { return nil })
		select {
		case <-c.done:
		case <-time.After(10 * time.Second):
			t.Fatal("agent did not exit")
		}
		err := c.sendRaw(t.Context(), []byte(`{}`))
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("run %d: write to a dead agent = %v, want the pipe's error", i, err)
		}
	}
}
