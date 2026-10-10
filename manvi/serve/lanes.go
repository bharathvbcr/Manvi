package serve

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Dispatch model.
//
// The read loop never runs an operation that can block. Cheap operations —
// hello, the policy checks, chat bookkeeping, and stopping a managed run — are
// answered inline, in arrival order. Everything else runs on a lane: one
// goroutine per operation family, taking its requests in arrival order.
//
// This replaced fully serial dispatch, under which one slow call held every
// other one: a capability.probe allowed ten minutes, or a managed.prepare
// waiting on a provider's handshake, and work.runs.managed.stop — an instant
// cancel — sat unanswered behind it. A host that timed out then tore the whole
// sidecar down, taking every managed session it hosted with it.
//
// Lanes keep the two properties serial dispatch had for free:
//
//   - Order within a family. Two workbench writes a host pipelines still run in
//     the order it sent them, so nothing that relied on serial execution of
//     store operations can observe a reordering.
//   - A bound. The number of goroutines is the number of lanes, fixed at
//     build time, and each lane queues at most laneQueue requests. A request
//     that finds its lane's queue full is answered E_BUSY at once rather than
//     buffered without limit.
//
// Responses are correlated by id, as protocol.go has always stated: a host
// with calls on two lanes can see the later one answered first.
//
// Each lane request runs under its own deadline, derived from the server
// context, so a stuck dependency costs one request rather than its lane
// forever. Work an operation hands to a background job — a managed run, an
// enhancement — runs on its own context and is not bounded by the request's.

// laneQueue is how many requests one lane holds beyond the one it is running.
const laneQueue = 32

// laneFor names the lane op runs on, or "" for inline.
func laneFor(op string) string {
	switch op {
	case OpHello, OpPolicyCheckFile, OpPolicyCheckCommand, OpChatPrepare, OpChatSettle, OpChatForget,
		opManagedStop:
		return ""
	case OpCapabilityProbe, OpLocalScan:
		return "local"
	}
	switch {
	case op == OpWorkRefine:
		// One model call that outlives the work lane's minute: on its own lane it
		// cannot hold the board reads and writes queued behind it.
		return "refine"
	case strings.HasPrefix(op, "devmap."):
		return "devmap"
	case strings.HasPrefix(op, "work.runs.managed."):
		return "managed"
	case strings.HasPrefix(op, "work."):
		return "work"
	default:
		return "module"
	}
}

// laneOf is laneFor for an op this server serves, and inline for one it does
// not, whose answer is the immediate unknown-op refusal.
func (s *Server) laneOf(op string) string {
	if _, ok := s.router.handlers[op]; !ok {
		return ""
	}
	return laneFor(op)
}

// opManagedStop is answered inline: it is an instant cancel, and the request it
// exists to interrupt may be the one occupying the managed lane.
const opManagedStop = "work.runs.managed.stop"

// deadlineFor bounds one lane request. A variable so a test can shorten it.
var deadlineFor = opDeadline

// opDeadline is each operation's own worst case with headroom, so the deadline
// only ever ends a request that is stuck.
func opDeadline(op string) time.Duration {
	switch {
	case op == OpCapabilityProbe:
		return time.Duration(maxProbeTimeoutMS)*time.Millisecond + 30*time.Second
	case op == OpLocalScan:
		return 45 * time.Second
	case op == "work.runs.managed.prepare":
		// Claim and provider start: two bounded store calls, a version probe
		// and a handshake of up to 30s. The job itself outlives this.
		return 3 * time.Minute
	case op == OpWorkRefine:
		// The model call (enhancementJobTimeout) plus the grounding and board reads around it.
		return enhancementJobTimeout + 30*time.Second
	case strings.HasPrefix(op, "devmap."), strings.HasPrefix(op, "work."):
		return time.Minute
	default:
		return 2 * time.Minute
	}
}

type laneJob struct {
	ctx context.Context
	req Request
}

// lanes runs lane requests. The zero value is unusable; build with newLanes.
type lanes struct {
	s      *Server
	mu     sync.Mutex
	queues map[string]chan laneJob
	closed bool
	wg     sync.WaitGroup
}

func newLanes(s *Server) *lanes {
	return &lanes{s: s, queues: map[string]chan laneJob{}}
}

// submit queues req on its lane, starting the lane on first use. It reports
// false when the lane is full or the server is draining; the caller answers
// E_BUSY.
func (l *lanes) submit(ctx context.Context, name string, req Request) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return false
	}
	q, ok := l.queues[name]
	if !ok {
		q = make(chan laneJob, laneQueue)
		l.queues[name] = q
		l.wg.Add(1)
		go l.run(q)
	}
	select {
	case q <- laneJob{ctx: ctx, req: req}:
		return true
	default:
		return false
	}
}

func (l *lanes) run(q chan laneJob) {
	defer l.wg.Done()
	for job := range q {
		ctx, cancel := context.WithTimeout(job.ctx, deadlineFor(job.req.Op))
		l.s.dispatch(ctx, job.req)
		cancel()
		l.s.flushLane()
	}
}

// drain stops accepting work and waits for every queued request to be
// answered. A closed stdin means the host has sent everything it will send,
// not that it stopped listening: a pipeline that writes its requests and then
// closes its end still reads every answer.
func (l *lanes) drain() {
	l.mu.Lock()
	if !l.closed {
		l.closed = true
		for _, q := range l.queues {
			close(q)
		}
	}
	l.mu.Unlock()
	l.wg.Wait()
}

// busy is the answer to a request its lane cannot take.
func busy(req Request, lane string) Response {
	return Response{ID: req.ID, OK: false, Error: &Error{
		Code:      ErrBusy,
		Message:   fmt.Sprintf("the %s lane already has %d requests waiting; retry when an earlier one is answered", lane, laneQueue),
		Retryable: true,
	}}
}
