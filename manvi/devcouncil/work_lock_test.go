package devcouncil

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// parkedBoard answers every liveness check and parks every Workbench call
// until released, standing for a store that is slow to answer. entered is
// closed when the first Workbench call arrives.
type parkedBoard struct {
	release chan struct{}
	entered chan struct{}
	once    sync.Once

	mu     sync.Mutex
	valids int
	valid2 chan struct{}
}

func (b *parkedBoard) Workbench(ctx context.Context, _ string, _ json.RawMessage) (json.RawMessage, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *parkedBoard) Valid(context.Context, string, string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.valids++
	if b.valids == 2 {
		close(b.valid2)
	}
	return true, nil
}

// The wait for the lock is bounded too: a create that cannot get it in time
// refuses as busy and writes nothing.
func TestWorkCreateWaitingForTheRepositoryLockTimesOut(t *testing.T) {
	board := &parkedBoard{release: make(chan struct{}), entered: make(chan struct{}), valid2: make(chan struct{})}
	api, err := newWorkAPI(board, WorkBinding{RunID: "lockrun"})
	if err != nil {
		t.Fatal(err)
	}
	api.repo = localRepositoryID
	api.lockWait = 100 * time.Millisecond
	create := func(ctx context.Context) tools.Result {
		sess := &Session{}
		sess.adopt("task-1", "token", "owner")
		raw, _ := json.Marshal(map[string]string{"title": "a proposal"})
		return api.create(ctx, sess, tools.Call{ID: "c", Arguments: raw})
	}
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		create(context.Background())
	}()
	t.Cleanup(func() {
		close(board.release)
		<-firstDone
	})
	select {
	case <-board.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first create never reached the board")
	}
	second := make(chan tools.Result, 1)
	go func() { second <- create(context.Background()) }()
	select {
	case res := <-second:
		if !res.IsError || !strings.Contains(res.Text, "busy") {
			t.Fatalf("a create that could not get the lock did not refuse as busy: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the wait for the repository lock was not bounded")
	}
}

// The repository lock is held across the store calls of a create. A second
// create waiting for it has to be able to give up when its call is cancelled; a
// bare sync.Mutex.Lock cannot, so the cancelled call sat behind a slow store
// until the first create finished.
func TestWorkCreateWaitingForTheRepositoryLockHonoursCancellation(t *testing.T) {
	board := &parkedBoard{release: make(chan struct{}), entered: make(chan struct{}), valid2: make(chan struct{})}
	api, err := newWorkAPI(board, WorkBinding{RunID: "lockrun"})
	if err != nil {
		t.Fatal(err)
	}
	api.repo = localRepositoryID

	create := func(ctx context.Context) tools.Result {
		sess := &Session{}
		sess.adopt("task-1", "token", "owner")
		raw, _ := json.Marshal(map[string]string{"title": "a proposal"})
		return api.create(ctx, sess, tools.Call{ID: "c", Arguments: raw})
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		create(context.Background())
	}()
	t.Cleanup(func() {
		close(board.release)
		select {
		case <-firstDone:
		case <-time.After(30 * time.Second):
			t.Error("the first create never finished after the board was released")
		}
	})
	select {
	case <-board.entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the first create never reached the board")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	second := make(chan tools.Result, 1)
	go func() { second <- create(ctx) }()
	select {
	case <-board.valid2:
	case <-time.After(30 * time.Second):
		t.Fatal("the second create never passed its liveness check")
	}
	// It is past every check that reads ctx and is now waiting for the lock.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case res := <-second:
		if !res.IsError || !strings.Contains(res.Text, "cancelled") {
			t.Fatalf("the cancelled create did not report cancellation: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled create stayed blocked on the repository lock while another create held it")
	}
}
