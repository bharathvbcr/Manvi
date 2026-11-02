package serve

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var errNoServerControl = errors.New("control module has no server to report on")

// The ops of a server that outlives its host.
//
// `manvi serve --listen` keeps running managed jobs after the program that
// started it has gone, so a later host must be able to ask what it is looking
// at before it adopts anything: whose server this is, which jobs are live, and
// which of them it may still drive. serve.status answers that. serve.stop_all is
// the explicit way to end every job without naming each. serve.takeover is the
// one way a second client replaces the first (socket.go has the rule).
//
// They are an optional module and are registered only for a socket-served
// process, so a stdio server's hello lists exactly the ops it always did.

// maxStopAllWait bounds how long serve.stop_all waits for the jobs it cancelled.
const maxStopAllWait = 30 * time.Second

// defaultStopAllWait is how long it waits when the request does not say.
const defaultStopAllWait = 5 * time.Second

// ServerControl is what a long-lived server offers its controlling client.
type ServerControl interface {
	// Status reports the server's identity and its live jobs.
	Status() ServeStatusResult
	// StopAll cancels every live managed job and waits, up to wait, for them to end.
	StopAll(ctx context.Context, wait time.Duration) ServeStopAllResult
}

// ControlModule registers serve.status, serve.stop_all and serve.takeover.
type ControlModule struct {
	Control ServerControl
}

// ServeJob is one live managed job, enough for a reconnecting host to match it
// to the run record it holds and decide whether to adopt it.
type ServeJob struct {
	// ID is the managed run's id, the one every work.runs.managed.* op names.
	ID string `json:"id"`
	// OwnerID and SessionID are the identities the job's run was claimed under.
	// OwnerID has the form manvi-{pid}-{nanos}-{token}, and its pid is this
	// server's: while that process lives, the job is not orphaned.
	OwnerID   string `json:"owner_id"`
	SessionID string `json:"session_id"`
	// ProcessID is the provider process, 0 until it has been started.
	ProcessID int `json:"process_id"`
	// Phase is preparing, awaiting_activation, running, or what the job ended as.
	Phase string `json:"phase"`
	// Provider names the coding agent, empty until the job has loaded its run.
	Provider string `json:"provider,omitempty"`
	// Activated says a host has activated the prepared process. A job that is not
	// activated is given up after five minutes; activating it is the adoption of
	// a preparation whose host went away between the two calls.
	Activated bool `json:"activated"`
	// Continue is the run's follow-up state, for a run prepared with
	// max_continuations. It is what a status read of work.runs.managed.continue
	// answers.
	Continue *ManagedContinueResult `json:"continue,omitempty"`
}

// ServeStatusResult is serve.status's answer.
type ServeStatusResult struct {
	Protocol int `json:"protocol"`
	// OwnerID is this server's identity in the form a managed claim records:
	// manvi-{pid}-{nanos}-{token}, stamped when the server started. A host that
	// reconciles stranded runs can ask whether that process is still the one it
	// started, as it does for any claim's owner.
	OwnerID   string `json:"owner_id"`
	ProcessID int    `json:"process_id"`
	// Controller says whether the connection that asked is the controlling client.
	// A connection that is not can read this and take over; it can do nothing else.
	Controller bool `json:"controller"`
	// Jobs are the managed jobs that have not ended, in id order. At most
	// MaxManagedSessions.
	Jobs []ServeJob `json:"jobs"`
	// IdleExitMS is how long the server waits with no client and no job before it
	// exits, in milliseconds.
	IdleExitMS int64 `json:"idle_exit_ms"`
}

// ServeStopAllParams is serve.stop_all's request.
type ServeStopAllParams struct {
	// WaitMS is how long to wait for the cancelled jobs to end, in milliseconds.
	// Absent or zero means five seconds; at most thirty.
	WaitMS int64 `json:"wait_ms,omitempty"`
}

// ServeStopAllResult is serve.stop_all's answer.
type ServeStopAllResult struct {
	// Stopped are the jobs that had ended when the wait was over.
	Stopped []string `json:"stopped"`
	// Pending are jobs that were cancelled and had not ended yet. They are ending:
	// read serve.status, or the run, to see when they have.
	Pending []string `json:"pending"`
}

// ServeTakeoverResult is serve.takeover's answer.
type ServeTakeoverResult struct {
	// Controller is true: whoever receives this answer controls the server.
	Controller bool `json:"controller"`
	// TookOver says a previous controlling client was replaced. False on a
	// connection that already controlled, and on one that found the slot free.
	TookOver bool `json:"took_over"`
}

// Configure implements Module.
func (m ControlModule) Configure(r *Router) error {
	if nilInterface(m.Control) {
		return errNoServerControl
	}
	if err := r.Register(OpServeStatus, func(context.Context, json.RawMessage) (any, *Error) {
		return m.Control.Status(), nil
	}); err != nil {
		return err
	}
	if err := r.Register(OpServeTakeover, func(context.Context, json.RawMessage) (any, *Error) {
		// A connection served by the router already controls the server; a
		// connection that does not is answered before the router (socket.go).
		return ServeTakeoverResult{Controller: true}, nil
	}); err != nil {
		return err
	}
	return r.Register(OpServeStopAll, func(ctx context.Context, raw json.RawMessage) (any, *Error) {
		var p ServeStopAllParams
		if len(raw) > 0 {
			if e := strictParams(raw, OpServeStopAll, &p); e != nil {
				return nil, e
			}
		}
		wait := defaultStopAllWait
		switch {
		case p.WaitMS < 0 || p.WaitMS > maxStopAllWait.Milliseconds():
			return nil, badRequest("wait_ms is between 0 and %d, not %d", maxStopAllWait.Milliseconds(), p.WaitMS)
		case p.WaitMS > 0:
			wait = time.Duration(p.WaitMS) * time.Millisecond
		}
		return m.Control.StopAll(ctx, wait), nil
	})
}
