package serve

import (
	"context"
	"encoding/json"
	"io"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingModule registers ops that hold their lane until released, and an
// instant stand-in for work.runs.managed.stop.
type blockingModule struct {
	release chan struct{}
	started chan string
	mu      sync.Mutex
	order   []string
}

func newBlockingModule() *blockingModule {
	return &blockingModule{release: make(chan struct{}), started: make(chan string, 1024)}
}

func (m *blockingModule) Configure(r *Router) error {
	block := func(ctx context.Context, raw json.RawMessage) (any, *Error) {
		var p struct {
			Tag string `json:"tag"`
		}
		_ = json.Unmarshal(raw, &p)
		m.mu.Lock()
		m.order = append(m.order, p.Tag)
		m.mu.Unlock()
		m.started <- p.Tag
		select {
		case <-m.release:
			return map[string]string{"tag": p.Tag}, nil
		case <-ctx.Done():
			return nil, &Error{Code: ErrDependency, Message: ctx.Err().Error(), Retryable: true}
		}
	}
	for _, op := range []string{"work.runs.managed.prepare", "work.items.update", "test.block"} {
		if err := r.Register(op, block); err != nil {
			return err
		}
	}
	return r.Register(opManagedStop, func(context.Context, json.RawMessage) (any, *Error) {
		return map[string]bool{"stopped": true}, nil
	})
}

// session drives a server over a pipe the test writes to, and collects its
// responses as they arrive.
type session struct {
	t   *testing.T
	in  *io.PipeWriter
	got chan Response
	// early holds responses read while waiting for a different id, in arrival
	// order per id, so awaiting ids in an order other than the one they were
	// answered in loses none, and a reused id loses neither answer.
	early map[string][]Response
	done  chan error
	srvMu sync.Mutex
}

func startSession(t *testing.T, ctx context.Context, opts Options) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, got: make(chan Response, 4096), done: make(chan error, 1)}
	srv := New(outW, opts)
	go func() {
		err := srv.Serve(ctx, inR)
		_ = outW.Close()
		s.done <- err
	}()
	go func() {
		dec := json.NewDecoder(outR)
		for {
			var r Response
			if err := dec.Decode(&r); err != nil {
				close(s.got)
				return
			}
			s.got <- r
		}
	}()
	return s
}

func (s *session) send(id, op string, params string) {
	s.t.Helper()
	req := Request{ID: id, Op: op}
	if params != "" {
		req.Params = json.RawMessage(params)
	}
	line, err := json.Marshal(req)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(line, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) await(id string, within time.Duration) Response {
	s.t.Helper()
	if held := s.early[id]; len(held) > 0 {
		if len(held) == 1 {
			delete(s.early, id)
		} else {
			s.early[id] = held[1:]
		}
		return held[0]
	}
	deadline := time.After(within)
	for {
		select {
		case r, ok := <-s.got:
			if !ok {
				s.t.Fatalf("stream ended before %q was answered", id)
			}
			if r.ID == id {
				return r
			}
			if s.early == nil {
				s.early = map[string][]Response{}
			}
			s.early[r.ID] = append(s.early[r.ID], r)
		case <-deadline:
			s.t.Fatalf("%q was not answered within %v", id, within)
		}
	}
}

// awaitAll collects the answer to every id in ids, whichever order they arrive in,
// and ignores any other answer it reads. await returns the first answer for ONE id
// and drops every other answer it reads past, so two calls that finish
// concurrently cannot be awaited one after the other with it: when the second
// finishes first, awaiting the first swallows its answer and the second await then
// times out on an answer that was already thrown away.
func (s *session) awaitAll(within time.Duration, ids ...string) map[string]Response {
	s.t.Helper()
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	got := make(map[string]Response, len(ids))
	deadline := time.After(within)
	for len(got) < len(want) {
		select {
		case r, ok := <-s.got:
			if !ok {
				s.t.Fatalf("stream ended with %d of %v answered", len(got), ids)
			}
			if want[r.ID] {
				got[r.ID] = r
			}
		case <-deadline:
			s.t.Fatalf("%d of %v were answered within %v", len(got), ids, within)
		}
	}
	return got
}

// The defect lanes exist for: an instant cancel waited behind a slow call.
// Under serial dispatch this stop was answered only after prepare returned.
func TestStopIsAnsweredWhilePrepareHoldsItsLane(t *testing.T) {
	m := newBlockingModule()
	s := startSession(t, context.Background(), Options{Modules: []Module{m}})
	defer close(m.release)
	s.send("prep", "work.runs.managed.prepare", `{"tag":"prep"}`)
	<-m.started
	start := time.Now()
	s.send("stop", opManagedStop, "")
	if r := s.await("stop", time.Second); !r.OK {
		t.Fatalf("stop failed: %+v", r.Error)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("stop answered after %v while prepare held the managed lane", elapsed)
	}
	for _, op := range []string{OpHello, OpPolicyCheckCommand} {
		s.send(op, op, `{"command":"true","root":"/tmp"}`)
		if r := s.await(op, time.Second); !r.OK && op == OpHello {
			t.Fatalf("%s failed: %+v", op, r.Error)
		}
	}
}

// Different families do not wait on each other.
func TestASlowLaneDoesNotHoldAnotherLane(t *testing.T) {
	m := newBlockingModule()
	s := startSession(t, context.Background(), Options{Modules: []Module{m}})
	s.send("prep", "work.runs.managed.prepare", `{"tag":"prep"}`)
	<-m.started
	s.send("item", "work.items.update", `{"tag":"item"}`)
	select {
	case tag := <-m.started:
		if tag != "item" {
			t.Fatalf("started %q, want item", tag)
		}
	case <-time.After(time.Second):
		t.Fatal("a work op waited behind a managed prepare")
	}
	close(m.release)
	// Two lanes answer in whichever order they finish, so read both ids from
	// one stream rather than awaiting them in a fixed order.
	for pending := 2; pending > 0; pending-- {
		select {
		case r := <-s.got:
			if r.ID != "prep" && r.ID != "item" {
				t.Fatalf("unexpected response %q", r.ID)
			}
		case <-time.After(time.Second):
			t.Fatalf("%d of prep and item were not answered", pending)
		}
	}
}

// Within one family, requests keep the order they arrived in, as they did
// under serial dispatch.
func TestALaneRunsItsRequestsInArrivalOrder(t *testing.T) {
	m := newBlockingModule()
	close(m.release)
	s := startSession(t, context.Background(), Options{Modules: []Module{m}})
	// No more than the lane can hold waiting. Past laneQueue the outcome is
	// scheduling, not ordering: an overflow is answered E_BUSY on the read loop,
	// ahead of the lane's own replies. TestAFullLaneIsBusyAndBounded owns the
	// overflow.
	const n = laneQueue
	for i := range n {
		tag := "t" + strings.Repeat("x", i)
		s.send(tag, "work.items.update", `{"tag":"`+tag+`"}`)
	}
	for i := range n {
		s.await("t"+strings.Repeat("x", i), 5*time.Second)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, tag := range m.order {
		if want := "t" + strings.Repeat("x", i); tag != want {
			t.Fatalf("lane ran %q at position %d, want %q", tag, i, want)
		}
	}
}

// A full lane answers E_BUSY at once, retryably, instead of queueing without
// limit; and the number of goroutines does not grow with the flood.
func TestAFullLaneIsBusyAndBounded(t *testing.T) {
	m := newBlockingModule()
	s := startSession(t, context.Background(), Options{Modules: []Module{m}})
	s.send("first", "test.block", `{"tag":"first"}`)
	<-m.started
	before := runtime.NumGoroutine()
	const flood = 2000
	for i := range flood {
		s.send("f"+strconv.Itoa(i), "test.block", `{}`)
	}
	busyCount := 0
	for i := range flood - laneQueue {
		r := s.await("f"+strconv.Itoa(laneQueue+i), 5*time.Second)
		if r.OK || r.Error == nil || r.Error.Code != ErrBusy || !r.Error.Retryable {
			t.Fatalf("request past the queue: %+v, want a retryable E_BUSY", r)
		}
		busyCount++
	}
	if grown := runtime.NumGoroutine() - before; grown > 5 {
		t.Fatalf("%d goroutines appeared for a flood of %d requests", grown, flood)
	}
	close(m.release)
	if busyCount != flood-laneQueue {
		t.Fatalf("busy = %d, want %d", busyCount, flood-laneQueue)
	}
	s.await("first", 5*time.Second)
	for i := range laneQueue {
		if r := s.await("f"+strconv.Itoa(i), 5*time.Second); !r.OK {
			t.Fatalf("queued request %d failed: %+v", i, r.Error)
		}
	}
}

// A stuck request ends at its deadline and its lane carries on.
func TestALaneRequestEndsAtItsDeadline(t *testing.T) {
	old := deadlineFor
	deadlineFor = func(string) time.Duration { return 50 * time.Millisecond }
	defer func() { deadlineFor = old }()
	m := newBlockingModule()
	s := startSession(t, context.Background(), Options{Modules: []Module{m}})
	s.send("stuck", "test.block", `{"tag":"stuck"}`)
	r := s.await("stuck", 2*time.Second)
	if r.OK || r.Error == nil || !strings.Contains(r.Error.Message, "deadline") {
		t.Fatalf("stuck request: %+v, want it ended by its deadline", r)
	}
	close(m.release)
	s.send("next", "test.block", `{"tag":"next"}`)
	if r := s.await("next", 2*time.Second); !r.OK {
		t.Fatalf("the lane did not recover: %+v", r.Error)
	}
}

// Cancelling the server ends the work in flight on every lane, and Serve
// returns once each accepted request has been answered.
func TestCancellingTheServerEndsLaneWork(t *testing.T) {
	m := newBlockingModule()
	ctx, cancel := context.WithCancel(context.Background())
	s := startSession(t, ctx, Options{Modules: []Module{m}})
	s.send("a", "test.block", `{"tag":"a"}`)
	s.send("b", "work.runs.managed.prepare", `{"tag":"b"}`)
	<-m.started
	<-m.started
	cancel()
	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// A host that writes its requests and closes stdin still reads every answer:
// end of input drains the lanes rather than abandoning them.
func TestEndOfInputDrainsEveryLane(t *testing.T) {
	m := newBlockingModule()
	close(m.release)
	var in strings.Builder
	ids := []string{}
	for i, op := range []string{"test.block", "work.items.update", "work.runs.managed.prepare", OpHello, "test.block"} {
		id := "r" + strconv.Itoa(i)
		ids = append(ids, id)
		line, _ := json.Marshal(Request{ID: id, Op: op, Params: json.RawMessage(`{"tag":"` + id + `"}`)})
		in.Write(append(line, '\n'))
	}
	responses := runLines(t, Options{Modules: []Module{m}}, in.String())
	for _, r := range byID(t, responses, ids...) {
		if !r.OK {
			t.Fatalf("%s: %+v", r.ID, r.Error)
		}
	}
}

// An unknown op is refused on the read loop. Queued on a lane it waited behind
// whatever held that lane, and could be answered after a request sent later.
func TestAnUnknownOpIsRefusedWithoutWaitingOnALane(t *testing.T) {
	m := newBlockingModule()
	defer close(m.release)
	s := startSession(t, context.Background(), Options{Modules: []Module{m}})
	s.send("hold", "test.block", `{"tag":"hold"}`)
	<-m.started
	start := time.Now()
	s.send("nope", "no.such.op", "")
	r := s.await("nope", time.Second)
	if r.OK || r.Error == nil || r.Error.Code != ErrBadRequest {
		t.Fatalf("unknown op: %+v, want E_BAD_REQUEST", r)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("an unknown op waited %v behind a busy lane", elapsed)
	}
}

// hello tells a host how long each op may take and on which lane, read from the
// same functions that enforce them, so a host's client timeout cannot drift
// from the server's deadline.
func TestHelloAdvertisesEachOpsLaneAndDeadline(t *testing.T) {
	m := newBlockingModule()
	resp := roundTrip(t, Options{Modules: []Module{m}}, Request{ID: "h", Op: OpHello, Params: json.RawMessage(`{"protocol":1}`)})[0]
	var hello HelloResult
	if err := json.Unmarshal(resp.Result, &hello); err != nil {
		t.Fatal(err)
	}
	if len(hello.OpLimits) != len(hello.Ops) {
		t.Fatalf("op_limits covers %d of %d ops", len(hello.OpLimits), len(hello.Ops))
	}
	for _, op := range hello.Ops {
		got, ok := hello.OpLimits[op]
		if !ok {
			t.Fatalf("op_limits omits %s", op)
		}
		lane := laneFor(op)
		switch {
		case lane == "" && (got.Lane != "inline" || got.DeadlineMS != 0):
			t.Errorf("%s is inline but advertised %+v", op, got)
		case lane != "" && (got.Lane != lane || got.DeadlineMS != opDeadline(op).Milliseconds()):
			t.Errorf("%s advertised %+v, enforced lane %q deadline %v", op, got, lane, opDeadline(op))
		}
	}
	if got := hello.OpLimits["work.runs.managed.prepare"]; got.Lane != "managed" || got.DeadlineMS != 180000 {
		t.Fatalf("prepare = %+v", got)
	}
	if laneFor(opManagedWake) != "" {
		t.Fatal("wake must be inline so a decision is not queued behind prepare")
	}
	if laneFor(opManagedFollow) != "" {
		t.Fatal("follow must be inline so a queued message is not parked behind the turn")
	}
}

// Awaiting ids in a different order than they were answered must not lose the
// earlier answers: the assist wire test awaits "one" and "two" while either may
// finish first.
func TestSessionAwaitKeepsResponsesForLaterAwaits(t *testing.T) {
	s := startSession(t, t.Context(), Options{})
	s.send("a", OpHello, "")
	s.send("b", OpHello, "")
	// Hello is answered inline, in arrival order, so "a" is read first here.
	if r := s.await("b", 5*time.Second); r.ID != "b" || !r.OK {
		t.Fatalf("b = %+v", r)
	}
	if r := s.await("a", 5*time.Second); r.ID != "a" || !r.OK {
		t.Fatalf("a = %+v", r)
	}
	// A reused id keeps both answers: neither overwrites the other while held.
	s.send("c", OpHello, "")
	s.send("c", OpHello, "")
	s.send("d", OpHello, "")
	if r := s.await("d", 5*time.Second); r.ID != "d" || !r.OK {
		t.Fatalf("d = %+v", r)
	}
	for i := range 2 {
		if r := s.await("c", 5*time.Second); r.ID != "c" || !r.OK {
			t.Fatalf("c #%d = %+v", i+1, r)
		}
	}
}
