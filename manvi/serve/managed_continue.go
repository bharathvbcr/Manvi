package serve

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// work.runs.managed.continue: another turn for a managed run.
//
// A run prepared with max_continuations > 0 does not end when a turn completes.
// Its drive loop checkpoints the run as still running, settles the turn's open
// callbacks, and parks, with the provider process kept alive. The host then
// sends the next message here; the drive loop writes a durable claim, delivers
// it with ManagedSession.ContinueTurn, and carries on reading the new turn
// exactly as it read the first. A run that was not prepared for it behaves as
// it always did: one turn, then it ends.
//
// What the store makes of this, because it decides the shape:
//
//   - The run stays `running` while it is parked. dc-store never lets a
//     provider_state leave `completed`, so a parked run must not be recorded as
//     completed; it is checkpointed `running` and the terminal state is written
//     once, when the run really ends. A host learns "parked" from this op's
//     status read (no message), not from the store.
//   - `provider_turn_id` is immutable once set, so the store keeps the FIRST
//     turn's id for the whole run. A later turn's id lives in the adapter and in
//     the decision rows its approvals create (decisions.create does not compare
//     it with the run's). Each approval still binds payload digest, run attempt,
//     owner and session, and the provider request id, and the adapter binds it
//     to the turn that raised it.
//   - Nothing in the store can record a follow-up. The durable claim is the
//     run's saved output: a line naming the follow-up's request id, size and
//     digest is written to it, and the checkpoint carrying it is acknowledged,
//     before the message is sent. A claim with no later evidence that the turn
//     began reads as unresolved.
//
// The rules a follow-up keeps (see codingagent/continue.go for the provider
// side): bounded in size, in turns per run, and in rate; refused while a turn
// is in flight; refused once the run ended; and never resent on uncertainty. An
// ambiguous delivery is reported as E_CONTINUE_UNRESOLVED, the run is recorded
// with provider_state unresolved, and the same request id returns the same
// answer instead of sending again.

// opManagedContinue is work.runs.managed.continue. It waits on the drive loop,
// which is the only caller of the provider, so it is not answered inline; and it
// has a lane of its own so it does not queue behind a prepare's handshake.
const opManagedContinue = "work.runs.managed.continue"

// Error codes of work.runs.managed.continue. A host may meet a code it does not
// know and handles it as a generic failure.
const (
	// ErrContinueUnsupported means the provider, or this run's preparation,
	// cannot take a follow-up turn. Not retryable.
	ErrContinueUnsupported = "E_CONTINUE_UNSUPPORTED"
	// ErrTurnInFlight means a turn is running, or the run is not between turns.
	// Retryable once the status read says parked.
	ErrTurnInFlight = "E_TURN_IN_FLIGHT"
	// ErrContinueLimit means the run took all the turns it was prepared for, or
	// its saved output has no room left for the claim. Not retryable.
	ErrContinueLimit = "E_CONTINUE_LIMIT"
	// ErrContinueRate means follow-ups came faster than one per
	// continueMinInterval. Retryable.
	ErrContinueRate = "E_CONTINUE_RATE"
	// ErrContinueUnresolved means a follow-up's delivery cannot be told from a
	// lost one. It is never resent and the run ends unresolved. Not retryable,
	// and the same request id answers the same way.
	ErrContinueUnresolved = "E_CONTINUE_UNRESOLVED"
	// ErrContinuePending means the wait for the delivery's outcome ran out before
	// it was known. Retrying with the same request id returns it.
	ErrContinuePending = "E_CONTINUE_PENDING"
	// ErrRunEnded means the run no longer takes follow-ups: it ended, was stopped,
	// or this host does not own it.
	ErrRunEnded = "E_RUN_ENDED"
)

var (
	// managedIdleBudget is how long a parked run waits for its next message
	// before it ends as completed. A variable so a test can shorten it.
	managedIdleBudget = 10 * time.Minute
	// continueMinInterval is the least time between two follow-ups of one run.
	continueMinInterval = 2 * time.Second
	// continueSendBudget bounds a delivery: the claim's checkpoint and the
	// provider write. It is under the op's own deadline, so a slow delivery is
	// answered by this budget and not by the lane.
	continueSendBudget = 30 * time.Second
)

// maxContinueBody bounds a request: a message of MaxContinueMessageBytes escapes
// to at most six bytes a byte in JSON, plus the other members.
const maxContinueBody = 6*codingagent.MaxContinueMessageBytes + 2048

// ManagedContinueParams is work.runs.managed.continue's request.
type ManagedContinueParams struct {
	// ID is the managed run.
	ID string `json:"id"`
	// RequestID identifies this follow-up. Required with a message. A retry of an
	// answered request id returns the first answer and sends nothing.
	RequestID string `json:"request_id,omitempty"`
	// Message is the follow-up: nonblank, at most
	// codingagent.MaxContinueMessageBytes. Absent makes the call a status read.
	Message *string `json:"message,omitempty"`
}

// ManagedContinueResult is work.runs.managed.continue's answer.
type ManagedContinueResult struct {
	// State is one of:
	//   delivered   the message was written to the provider; this does not say
	//               the new turn has begun, which the state read shows
	//   parked      between turns, ready for a message
	//   running     a turn is in flight; also reported before the first turn
	//               begins (turn 0) and while a finished turn is being settled,
	//               before it parks
	//   unresolved  an earlier delivery was ambiguous; no more are sent
	//   ended       the run takes no more follow-ups
	State     string `json:"state"`
	RequestID string `json:"request_id,omitempty"`
	// Turn is how many turns the run has started, the first included.
	Turn int `json:"turn"`
	// MaxTurns is how many it may start: the first plus max_continuations.
	MaxTurns int `json:"max_turns"`
	// IdleDeadline is when a parked run ends if nothing arrives, in Unix seconds.
	IdleDeadline int64 `json:"idle_deadline,omitempty"`
}

func continueUnsupported(format string, args ...any) *Error {
	return &Error{Code: ErrContinueUnsupported, Message: fmt.Sprintf(format, args...)}
}

func runEnded(format string, args ...any) *Error {
	return &Error{Code: ErrRunEnded, Message: fmt.Sprintf(format, args...)}
}

// continuationBudget decodes the optional max_continuations of a preparation.
func continuationBudget(raw json.RawMessage) (int, *Error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, nil
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, badRequest("max_continuations is a whole number")
	}
	if n < 0 || n > codingagent.MaxContinuations {
		return 0, badRequest("max_continuations is between 0 and %d, not %d", codingagent.MaxContinuations, n)
	}
	return n, nil
}

// continuation is a run's follow-up state. Guarded by the job's mu.
type continuation struct {
	max int
	// turns is how many turns have begun, the first included. A claim counts at
	// once, so two requests cannot both be admitted for one slot.
	turns      int
	parked     bool
	unresolved bool
	ended      bool
	last       time.Time
	idleUntil  time.Time
	// receipts is the answer to every request id the run has accepted and not
	// rolled back. At most max entries: each holds a turn.
	receipts map[string]*continueReceipt
	// requests carries the one request the parked drive loop is handed. Its
	// capacity is one because a claim leaves parked false until the loop is done.
	requests chan *continueRequest
}

func newContinuation(max int) *continuation {
	if max <= 0 {
		return nil
	}
	return &continuation{max: max, receipts: make(map[string]*continueReceipt), requests: make(chan *continueRequest, 1)}
}

type continueReceipt struct {
	digest string
	turn   int
	done   chan struct{}
	err    *Error
}

// finish records the outcome once. A nil error is a delivery.
func (rc *continueReceipt) finish(err *Error) {
	select {
	case <-rc.done:
		return
	default:
	}
	rc.err = err
	close(rc.done)
}

type continueRequest struct {
	id, message string
	// prevLast is the run's rate mark before this claim moved it, given back if
	// the follow-up is refused having written nothing.
	prevLast time.Time
	receipt  *continueReceipt
}

func (j *managedJob) startedFirstTurn() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.cont != nil {
		j.cont.turns = 1
	}
}

// canContinue reports whether the run may park after a completed turn.
func (j *managedJob) canContinue() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := j.cont
	return c != nil && !c.unresolved && !c.ended && c.turns < 1+c.max
}

// endContinuation closes the run to follow-ups for good and answers a request
// that was claimed in the same instant. Called as the drive loop leaves.
func (j *managedJob) endContinuation() {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := j.cont
	if c == nil {
		return
	}
	c.ended, c.parked = true, false
	select {
	case req := <-c.requests:
		req.receipt.finish(runEnded("the managed run ended before the follow-up was sent; nothing was sent"))
	default:
	}
}

// Continue is work.runs.managed.continue. With a message it delivers one
// follow-up to a parked run; without one it reports the run's state.
func (r *ManagedRunner) Continue(ctx context.Context, raw json.RawMessage) (ManagedContinueResult, error) {
	if len(raw) > maxContinueBody || !utf8.Valid(raw) {
		return ManagedContinueResult{}, badRequest("managed continue params exceed their byte limit or contain invalid Unicode")
	}
	var in ManagedContinueParams
	if e := strictParams(raw, opManagedContinue, &in); e != nil {
		return ManagedContinueResult{}, e
	}
	if !managedID(in.ID) {
		return ManagedContinueResult{}, badRequest("managed continue requires a managed run identity")
	}
	r.mu.Lock()
	closed := r.closed
	job := r.jobs[in.ID]
	r.mu.Unlock()
	if closed {
		return ManagedContinueResult{}, runEnded("the managed runner is closed")
	}
	if job == nil {
		return ManagedContinueResult{}, runEnded("this host does not own that managed run")
	}
	if in.Message == nil {
		return job.continueStatus()
	}
	message := *in.Message
	switch {
	case !managedID(in.RequestID):
		return ManagedContinueResult{}, badRequest("a follow-up names its request_id")
	case strings.TrimSpace(message) == "" || len(message) > codingagent.MaxContinueMessageBytes || strings.ContainsRune(message, 0):
		return ManagedContinueResult{}, badRequest("a follow-up is one nonblank message of at most %d bytes", codingagent.MaxContinueMessageBytes)
	}
	digest := sha256.Sum256([]byte(message))
	return r.claimContinuation(ctx, job, in.RequestID, message, hex.EncodeToString(digest[:]))
}

func (j *managedJob) continueStatus() (ManagedContinueResult, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	c := j.cont
	if c == nil {
		return ManagedContinueResult{}, continueUnsupported("this run was prepared without max_continuations")
	}
	res := ManagedContinueResult{Turn: c.turns, MaxTurns: 1 + c.max}
	select {
	case <-j.done:
		c.ended = true
	default:
	}
	switch {
	case c.unresolved:
		res.State = "unresolved"
	case c.ended:
		res.State = "ended"
	case c.parked:
		res.State = "parked"
		res.IdleDeadline = c.idleUntil.Unix()
	default:
		res.State = "running"
	}
	return res, nil
}

// claimContinuation admits one request and hands it to the parked drive loop.
// Everything that can refuse a follow-up without sending anything is decided
// here, under the job's lock, in one step with taking the slot.
func (r *ManagedRunner) claimContinuation(ctx context.Context, job *managedJob, requestID, message, digest string) (ManagedContinueResult, error) {
	job.mu.Lock()
	c := job.cont
	if c == nil {
		job.mu.Unlock()
		return ManagedContinueResult{}, continueUnsupported("this run was prepared without max_continuations")
	}
	if rc := c.receipts[requestID]; rc != nil {
		job.mu.Unlock()
		if rc.digest != digest {
			return ManagedContinueResult{}, badRequest("request_id %q already carried a different follow-up", requestID)
		}
		return job.awaitReceipt(ctx, requestID, rc)
	}
	select {
	case <-job.done:
		c.ended = true
	default:
	}
	now := time.Now()
	var refusal *Error
	switch {
	case c.unresolved:
		refusal = &Error{Code: ErrContinueUnresolved, Message: "an earlier follow-up's delivery is unresolved, so no further follow-up is sent"}
	case c.ended:
		refusal = runEnded("the managed run no longer takes follow-ups")
	case c.turns >= 1+c.max:
		// Before the in-flight check: a run on its last turn never parks again, so
		// waiting for it would be waiting for the end of the run.
		refusal = &Error{Code: ErrContinueLimit, Message: fmt.Sprintf("the run took all %d turns it was prepared for and ends when the last one completes", 1+c.max)}
	case !c.parked:
		refusal = &Error{Code: ErrTurnInFlight, Message: "a turn is still in flight; read the run's state and send the follow-up when it is parked", Retryable: true}
	case !c.last.IsZero() && now.Sub(c.last) < continueMinInterval:
		refusal = &Error{Code: ErrContinueRate, Message: fmt.Sprintf("follow-ups of one run are at least %s apart", continueMinInterval), Retryable: true}
	}
	if refusal != nil {
		job.mu.Unlock()
		return ManagedContinueResult{}, refusal
	}
	rc := &continueReceipt{digest: digest, turn: c.turns + 1, done: make(chan struct{})}
	prevLast := c.last
	c.parked, c.last = false, now
	c.turns++
	c.receipts[requestID] = rc
	// Never blocks: parked was true, so the channel is empty, and it stays
	// false until the loop has finished with this request.
	c.requests <- &continueRequest{id: requestID, message: message, receipt: rc, prevLast: prevLast}
	job.mu.Unlock()
	return job.awaitReceipt(ctx, requestID, rc)
}

func (j *managedJob) awaitReceipt(ctx context.Context, requestID string, rc *continueReceipt) (ManagedContinueResult, error) {
	select {
	case <-rc.done:
		if rc.err != nil {
			return ManagedContinueResult{}, rc.err
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		return ManagedContinueResult{State: "delivered", RequestID: requestID, Turn: rc.turn, MaxTurns: 1 + j.cont.max}, nil
	case <-ctx.Done():
		return ManagedContinueResult{}, &Error{Code: ErrContinuePending, Retryable: true,
			Message: fmt.Sprintf("the outcome of follow-up %q is not known yet; ask again with the same request_id, which never sends it twice", requestID)}
	}
}

// turnContext is a follow-up turn's own active-time budget, derived from the
// job so Stop and Close still end it. A function, so that the drive loop's
// single deferred cancel covers whichever turn is current.
func turnContext(job *managedJob) (context.Context, context.CancelFunc) {
	return context.WithTimeout(job.ctx, managedTurnBudget)
}

type continueOutcome int

const (
	continueDelivered  continueOutcome = iota // resume reading the new turn
	continueRefused                           // nothing sent; stay parked
	continueEnded                             // nothing sent; the run is over
	continueUnresolved                        // ambiguous; the run is over
)

// awaitContinuation parks a run whose turn just completed. It returns resumed
// when a follow-up was delivered and a new turn is running; with neither
// resumed nor an error the run ends as completed (idle budget spent, stopped,
// or its session closed); unresolved says the error is an ambiguous delivery.
//
// Only the drive loop calls it, so the provider keeps a single caller.
func (r *ManagedRunner) awaitContinuation(job *managedJob, thread string, turn *string) (resumed, unresolved bool, err error) {
	c := job.cont
	if job.ctx.Err() != nil {
		return false, false, nil
	}
	// The finished turn's callbacks are settled and its text is checkpointed
	// before anything can be sent: a follow-up starts from a record that already
	// says what the last turn left.
	storeCtx, stop := context.WithTimeout(job.ctx, continueSendBudget)
	err = r.settleCallbacks(storeCtx, job, "The turn ended before this request was answered")
	if err == nil {
		err = r.protocol(storeCtx, job, "running", thread, turn)
	}
	stop()
	if err != nil {
		if job.ctx.Err() != nil {
			return false, false, nil
		}
		return false, false, err
	}
	idle := time.NewTimer(managedIdleBudget)
	defer idle.Stop()
	job.mu.Lock()
	c.parked, c.idleUntil = true, time.Now().Add(managedIdleBudget)
	job.mu.Unlock()
	for {
		var req *continueRequest
		select {
		case req = <-c.requests:
		case <-idle.C:
		case <-job.ctx.Done():
		}
		if req == nil {
			job.mu.Lock()
			if c.parked {
				c.parked, c.ended = false, true
				job.mu.Unlock()
				return false, false, nil
			}
			// A claim landed beside the deadline; it was queued under this lock.
			select {
			case req = <-c.requests:
			default:
			}
			job.mu.Unlock()
			if req == nil {
				return false, false, nil
			}
		}
		outcome, deliverErr := r.deliver(job, c, req, thread, turn)
		switch outcome {
		case continueDelivered:
			return true, false, nil
		case continueUnresolved:
			return false, true, deliverErr
		case continueEnded:
			return false, false, nil
		}
	}
}

// deliver sends one claimed follow-up: durable claim first, then the write.
func (r *ManagedRunner) deliver(job *managedJob, c *continuation, req *continueRequest, thread string, turn *string) (continueOutcome, error) {
	n := req.receipt.turn - 1
	ctx, stop := context.WithTimeout(job.ctx, continueSendBudget)
	defer stop()
	claim := fmt.Sprintf("\n[follow-up %d claimed: request %q, %d bytes, sha256 %s]\n", n, req.id, len(req.message), req.receipt.digest)
	if job.output.Len()+len(claim) > maxManagedOutput {
		return r.refuseFollowUp(job, c, req, n, Error{Code: ErrContinueLimit, Message: "the run's saved output has no room for the claim, so nothing was sent"}, false), nil
	}
	job.output.WriteString(claim)
	// The claim is durable once this checkpoint is acknowledged. If it is not,
	// the store may or may not hold the line, but nothing was sent either way,
	// and the note below says so.
	if err := r.protocol(ctx, job, "running", thread, turn); err != nil {
		e := *dependencyError("the managed run store", err)
		e.Message += " (the follow-up's claim could not be recorded, so nothing was sent)"
		return r.refuseFollowUp(job, c, req, n, e, job.ctx.Err() != nil), nil
	}
	err := job.provider.ContinueTurn(ctx, req.message)
	switch {
	case err == nil:
		req.receipt.finish(nil)
		return continueDelivered, nil
	case codingagent.ContinueRefused(err):
		e, ends := continueRefusalError(err)
		return r.refuseFollowUp(job, c, req, n, e, ends || job.ctx.Err() != nil), nil
	}
	// A frame may have reached the provider. It is not sent again: the claim
	// stands in the record, the run ends unresolved, and the request id answers
	// the same way from now on.
	appendNote(job, fmt.Sprintf("\n[follow-up %d delivery unresolved: it may or may not have reached the provider and is not sent again]\n", n))
	job.mu.Lock()
	c.unresolved, c.parked = true, false
	job.mu.Unlock()
	req.receipt.finish(&Error{Code: ErrContinueUnresolved, Message: fmt.Sprintf("follow-up %q may or may not have been delivered and will not be sent again; the run ends unresolved", req.id)})
	return continueUnresolved, fmt.Errorf("follow-up %d delivery is unresolved: %w", n, err)
}

// refuseFollowUp gives the slot back after a follow-up that wrote nothing, so
// the host may offer it again, or ends the run when it cannot take another.
//
// The refusal is a value, not a pointer, so that every caller hands over a
// refusal that cannot be nil.
func (r *ManagedRunner) refuseFollowUp(job *managedJob, c *continuation, req *continueRequest, n int, e Error, ends bool) continueOutcome {
	appendNote(job, fmt.Sprintf("\n[follow-up %d not sent: %s]\n", n, e.Message))
	job.mu.Lock()
	c.turns--
	// Nothing was written, so the attempt does not use up the rate window.
	c.last = req.prevLast
	delete(c.receipts, req.id)
	if ends {
		c.parked, c.ended = false, true
	} else {
		c.parked = true
	}
	job.mu.Unlock()
	req.receipt.finish(&e)
	if ends {
		return continueEnded
	}
	return continueRefused
}

// continueRefusalError maps an adapter refusal to the wire error, and reports
// whether the run can take no further follow-up after it.
//
// Only the causes this names are retryable, and the fallback is not: it does not
// know why the adapter refused, so it must not say a retry would help.
func continueRefusalError(err error) (Error, bool) {
	switch {
	case errors.Is(err, codingagent.ErrSessionClosed):
		return *runEnded("the managed session is closed; nothing was sent"), true
	case errors.Is(err, codingagent.ErrContinuationHalted):
		return Error{Code: ErrContinueUnresolved, Message: "an earlier follow-up's delivery is unresolved; nothing was sent"}, true
	case errors.Is(err, codingagent.ErrContinueUnsupported):
		return *continueUnsupported("this provider cannot take a follow-up turn; nothing was sent"), false
	case errors.Is(err, codingagent.ErrNoCompletedTurn):
		return *continueUnsupported("the session has no completed turn to follow; nothing was sent"), false
	case errors.Is(err, codingagent.ErrContinueLimit):
		return Error{Code: ErrContinueLimit, Message: "the session took all the turns it may; nothing was sent"}, false
	case errors.Is(err, codingagent.ErrContinueMessage):
		return *badRequest("the provider refused the follow-up as blank or oversized; nothing was sent"), false
	case errors.Is(err, codingagent.ErrTurnInFlight):
		return Error{Code: ErrTurnInFlight, Message: "the provider still has a turn, or events of the last one, in flight; nothing was sent", Retryable: true}, false
	case errors.Is(err, codingagent.ErrProviderRejected):
		// An answer, not a transient fault: the provider said it started nothing.
		return Error{Code: ErrDependency, Message: "the provider rejected the follow-up and started no turn; nothing was sent"}, false
	case errors.Is(err, context.DeadlineExceeded):
		return Error{Code: ErrContinuePending, Message: "the delivery budget ran out before anything was sent", Retryable: true}, false
	case errors.Is(err, context.Canceled):
		// The job's context ended: the run is being stopped or closed.
		return *runEnded("the run is stopping; nothing was sent"), true
	}
	return Error{Code: ErrDependency, Message: "the provider refused the follow-up for a reason this host does not know; nothing was sent"}, false
}

// appendNote adds a line to the run's saved output when it fits. The claim line
// is the one that must fit; these are the record of what became of it.
func appendNote(job *managedJob, note string) {
	if job.output.Len()+len(note) <= maxManagedOutput {
		job.output.WriteString(note)
		return
	}
	job.truncated = true
}

// settleCallbacks resolves every callback the run still holds: a decision that
// was written to the provider is recorded as delivered, any other as cancelled
// for the given reason. The map is emptied so a parked run holds none.
func (r *ManagedRunner) settleCallbacks(ctx context.Context, job *managedJob, cancelled string) error {
	var err error
	for id, callback := range job.callbacks {
		state, reason := "cancelled", cancelled
		if callback.delivered {
			// Retries the receipt written at delivery, which is a no-op once the
			// row is resolved.
			state, reason = "resolved", deliveredReason
		}
		if resolveErr := r.resolve(ctx, job, callback, state, reason); resolveErr != nil {
			err = errors.Join(err, resolveErr)
			continue
		}
		delete(job.callbacks, id)
	}
	return err
}
