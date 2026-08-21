package devcouncil

import (
	"context"
	"sync"

	"manvi/agents"
)

// Session is the state one agent carries while working a task: which task it
// holds, and the token proving it.
//
// It lives here rather than in the tool arguments because a lease token is a
// credential. Passing it through the model's context would put it in the
// session log, in every subsequent prompt, and in any transcript the log is
// projected into — where it could be replayed by anything that reads them.
//
// There is one of these per *agent*, not one per process. That distinction is
// load-bearing and was the defect this type was reshaped to remove. A Registry
// used to hold a single Session that every concurrently-running sub-agent wrote
// through, so eight children checking out eight different tasks raced on one
// TaskID and one Token. Measured under the race detector: four reported races,
// and a child that had checked out TASK-C running its verification against
// TASK-E because a sibling's checkout had landed in between. The lease itself
// was never the problem — the store's partial unique index does its job — the
// problem was that the harness's own record of which lease it held was shared.
type Session struct {
	mu     sync.RWMutex
	taskID string
	token  string
	// owner identifies this agent to the store.
	owner string
	// inherited marks a session copied from a dispatching agent rather than
	// acquired by this one. A child inherits its parent's lease so its writes
	// are judged against the task the parent checked out — but it does not own
	// that lease, and must not be able to release or renew it out from under
	// the agent that does.
	inherited bool
	// leases, when set, is told about every lease this session acquires and
	// gives back. It is how a fan-out's cleanup learns about a lease a child
	// took by calling devcouncil_checkout_task itself, which is the only way
	// a child ever takes one. Without it a cancelled or finished child left
	// its task locked for the whole TTL and the fan-out reported itself clean.
	leases LeaseSink
}

// LeaseSink records the leases one agent holds, so something outside that agent
// can give them back when the agent cannot.
//
// It is declared here, as the narrowest thing that crosses, rather than by
// importing the fan-out package: this package describes what a lease-holding
// agent owes, and has no opinion about worker pools.
type LeaseSink interface {
	// HoldLease records a lease this agent has just acquired. It is called
	// before the agent does any work under that lease, so a cancellation always
	// finds it.
	HoldLease(taskID, token string)
	// DropLease forgets a lease this agent gave back itself, so cleanup does
	// not try again.
	DropLease(taskID string)
}

// SessionState is a Session read out as plain values, for a caller that needs
// to look at one without holding it.
type SessionState struct {
	TaskID    string
	Token     string
	Owner     string
	Inherited bool
}

// State returns what this session currently holds.
func (s *Session) State() SessionState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return SessionState{TaskID: s.taskID, Token: s.token, Owner: s.owner, Inherited: s.inherited}
}

// adopt records a lease this agent has taken, and tells the sink before the
// agent gets a chance to do anything under it.
func (s *Session) adopt(taskID, token, owner string) {
	s.mu.Lock()
	s.taskID, s.token, s.owner, s.inherited = taskID, token, owner, false
	sink := s.leases
	s.mu.Unlock()
	if sink != nil {
		sink.HoldLease(taskID, token)
	}
}

// clear forgets a lease this agent has given back.
func (s *Session) clear() {
	s.mu.Lock()
	taskID := s.taskID
	s.taskID, s.token, s.inherited = "", "", false
	sink := s.leases
	s.mu.Unlock()
	if sink != nil && taskID != "" {
		sink.DropLease(taskID)
	}
}

// NewChildSession returns the session one dispatched sub-agent runs under.
//
// It starts as a copy of the dispatching agent's, so a child fanned out under
// a checked-out task is judged against that task's scope exactly as its parent
// is — which is the case the shared session got right and the only reason it
// lasted. What changes is that the copy is the child's own: a checkout the
// child performs replaces the child's record and nothing else, so siblings
// cannot see it and cannot overwrite it.
//
// The copy is marked inherited. A child holding its parent's token could
// otherwise release or renew a lease it does not own, and the parent's own
// record would go on saying it holds a task the store has already given away.
func (s *Session) NewChildSession(leases LeaseSink) *Session {
	state := s.State()
	child := &Session{owner: state.Owner, leases: leases}
	if state.TaskID != "" && state.Token != "" {
		child.taskID, child.token, child.inherited = state.TaskID, state.Token, true
	}
	return child
}

type sessionKeyType struct{}

// sessionKey carries the acting agent's session on the context a tool call is
// dispatched with.
//
// The context is the seam because it is the only thing that already reaches
// every handler and is already per-call. The alternative — a second Registry
// per child — would have to reproduce the tool pipeline, its bus, and its gate
// stages, and two registries built from one set of Deps is a second answer to
// every question this one answers.
var sessionKey sessionKeyType

// WithSession attaches the session a tool call is to run under.
//
// Calls dispatched without one run under the Registry's own session, which is
// the top-level agent's. That is the pre-existing behaviour for every caller
// that is not a sub-agent, so nothing has to be threaded through the faces.
func WithSession(ctx context.Context, s *Session) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionKey, s)
}

// sessionFor returns the session the calling agent acts under.
func (r *Registry) sessionFor(ctx context.Context) *Session {
	if ctx != nil {
		if s, ok := ctx.Value(sessionKey).(*Session); ok && s != nil {
			return s
		}
	}
	return r.session
}

// holderSink lets a fan-out's per-child lease bookkeeping stand in as a
// LeaseSink.
//
// agents.Holder already records exactly what a cancelled or finished child
// still holds, and agents.Pool already gives those back on a fresh context —
// the machinery was complete and simply had nothing feeding it from the one
// path a child actually takes a lease on. This is that feed.
type holderSink struct{ h *agents.Holder }

func (s holderSink) HoldLease(taskID, token string) {
	s.h.Add(agents.Lease{TaskID: taskID, Token: token})
}

func (s holderSink) DropLease(taskID string) { s.h.Drop(taskID) }

// leaseSinkFor returns the sink one child reports its leases to.
func leaseSinkFor(h *agents.Holder) LeaseSink {
	if h == nil {
		return nil
	}
	return holderSink{h: h}
}
