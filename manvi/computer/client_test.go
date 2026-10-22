package computer

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"sync"
	"testing"
	"time"
)

type blockedPipe struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (p *blockedPipe) Write(_ []byte) (int, error) {
	close(p.entered)
	<-p.closed
	return 0, io.ErrClosedPipe
}
func (p *blockedPipe) Close() error { p.once.Do(func() { close(p.closed) }); return nil }
func TestBlockedNativeWriteCannotIgnoreCancellation(t *testing.T) {
	p := &blockedPipe{entered: make(chan struct{}), closed: make(chan struct{})}
	done := make(chan struct{})
	close(done)
	c := &Client{cmd: &exec.Cmd{}, in: p, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: done}
	ctx, cancel := context.WithCancel(context.Background())
	returned := make(chan error, 1)
	go func() { _, err := c.Call(ctx, "observe", nil, nil); returned <- err }()
	<-p.entered
	cancel()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("cancelled write returned success")
		}
	case <-time.After(safetyTimeout):
		p.Close()
		<-returned
		t.Fatal("blocked write ignored cancellation")
	}
}

// pipedClient is a Client wired to an in-process broker that answers every
// request with reply(op) and records the operations it saw.
func pipedClient(t *testing.T, reply func(op string) string) (*Client, *[]string) {
	t.Helper()
	reqR, reqW := io.Pipe()
	respR, respW := io.Pipe()
	c := &Client{cmd: &exec.Cmd{}, in: reqW, write: make(chan struct{}, 1), pending: map[string]chan pendingResult{}, done: make(chan struct{})}
	go c.read(respR)
	var ops []string
	go func() {
		sc := bufio.NewScanner(reqR)
		for sc.Scan() {
			var req request
			if json.Unmarshal(sc.Bytes(), &req) != nil {
				return
			}
			ops = append(ops, req.Op)
			out, _ := json.Marshal(response{ID: req.ID, OK: true, Result: json.RawMessage(reply(req.Op))})
			if _, err := respW.Write(append(out, '\n')); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { respW.Close(); reqR.Close() })
	return c, &ops
}

func TestActAndHumanActShareOneDispatch(t *testing.T) {
	c, ops := pipedClient(t, func(string) string { return `{"action_id":"a1","delivery":"sent"}` })
	ctx := context.Background()
	// A slice, not a map: the order of the calls is what the check below reads.
	for _, tc := range []struct {
		name string
		call func(context.Context, Session, Action) (Receipt, error)
	}{{"act", c.Act}, {"human_act", c.HumanAct}} {
		r, err := tc.call(ctx, Session{ID: "s"}, Action{ActionID: "a1"})
		if err != nil || r.ActionID != "a1" {
			t.Fatalf("%s: receipt=%+v err=%v", tc.name, r, err)
		}
	}
	if len(*ops) != 2 || (*ops)[0] != "act" || (*ops)[1] != "human_act" {
		t.Fatalf("operations = %v", *ops)
	}
}

func TestReceiptForAnotherActionIsRefused(t *testing.T) {
	c, _ := pipedClient(t, func(string) string { return `{"action_id":"someone-else"}` })
	ctx := context.Background()
	if _, err := c.Act(ctx, Session{ID: "s"}, Action{ActionID: "a1"}); err == nil || err.Error() != "action receipt identity mismatch" {
		t.Fatalf("Act err = %v", err)
	}
	if r, err := c.HumanAct(ctx, Session{ID: "s"}, Action{ActionID: "a1"}); err == nil || err.Error() != "human receipt identity mismatch" || r.ActionID != "" {
		t.Fatalf("HumanAct receipt=%+v err=%v", r, err)
	}
}
