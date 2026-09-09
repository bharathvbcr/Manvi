package computer

import (
	"context"
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
	case <-time.After(100 * time.Millisecond):
		p.Close()
		<-returned
		t.Fatal("blocked write ignored cancellation")
	}
}
