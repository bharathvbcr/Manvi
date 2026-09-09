package computer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

type Desktop interface {
	Observe(context.Context, Session) (Observation, error)
	Resolve(context.Context, Session, string, Selector) (Node, error)
	Act(context.Context, Session, Action) (Receipt, error)
	Approve(context.Context, Session, Action) (string, error)
	Control(context.Context, Session, string) (Session, error)
	Resume(context.Context, Session, string) (Session, error)
	HumanAct(context.Context, Session, Action) (Receipt, error)
}

// Focuser is required for native approval revalidation and explicit focus.
// It invalidates older observations without changing the session epoch.
type Focuser interface {
	Focus(context.Context, Session) (Session, error)
}
type Control struct {
	Kind          string `json:"kind"`
	ActionID      string `json:"action_id,omitempty"`
	ObservationID string `json:"observation_id,omitempty"`
	Epoch         uint64 `json:"epoch"`
	Action        Action `json:"action"`
}
type Record struct {
	Kind        string          `json:"kind"`
	Stage       string          `json:"stage,omitempty"`
	StepKind    string          `json:"step_kind,omitempty"`
	ActionID    string          `json:"action_id"`
	Actor       string          `json:"actor"`
	Epoch       uint64          `json:"epoch"`
	Observation *Observation    `json:"observation,omitempty"`
	Receipt     *Receipt        `json:"receipt,omitempty"`
	Event       *workflow.Event `json:"event,omitempty"`
	Reason      string          `json:"reason,omitempty"`
}
type RunOptions struct {
	// Assisted permits one read-only Back/Dismiss recovery while paused.
	Assisted bool
	Program  *workflow.Program
	Inputs   map[string]workflow.Value
	Desktop  Desktop
	Session  Session
	Privacy  PrivacyPolicy
	OnRecord func(Record) error
}
type RunResult struct {
	Initial workflow.State   `json:"initial"`
	State   workflow.State   `json:"state"`
	Events  []workflow.Event `json:"events"`
	Error   string           `json:"error,omitempty"`
}
type Run struct {
	control     chan Control
	done        chan struct{}
	mu          sync.RWMutex
	state       workflow.State
	observation *Observation
	result      RunResult
	err         error
}

func StartRun(ctx context.Context, opts RunOptions) (*Run, error) {
	if opts.Program == nil || opts.Desktop == nil {
		return nil, errors.New("program and desktop required")
	}
	if err := opts.Program.ValidateInputs(opts.Inputs); err != nil {
		return nil, err
	}
	initial, err := workflow.NewState(opts.Program, opts.Session.RunID, opts.Session.ID, opts.Session.Epoch)
	if err != nil {
		return nil, err
	}
	inputCopy := make(map[string]workflow.Value, len(opts.Inputs))
	for k, v := range opts.Inputs {
		inputCopy[k] = v
	}
	opts.Inputs = inputCopy
	opts.Privacy.Watched = append([]string(nil), opts.Privacy.Watched...)
	opts.Privacy.SensitiveTargets = ownedSelectors(opts.Privacy.SensitiveTargets)
	for _, parameter := range opts.Program.Capability().Parameters {
		if parameter.Sensitive {
			opts.Privacy.Watched = append(opts.Privacy.Watched, opts.Inputs[parameter.Name].Display())
		}
	}
	opts.Privacy.PID = opts.Session.Window.PID
	opts.Privacy.WindowID = opts.Session.Window.ID
	r := &Run{control: make(chan Control, 16), done: make(chan struct{}), state: initial}
	go r.execute(ctx, opts, initial)
	return r, nil
}
func (r *Run) Send(ctx context.Context, control Control) error {
	control.Action = ownedAction(control.Action)
	select {
	case <-r.done:
		return errors.New("run has ended")
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return errors.New("run has ended")
	case r.control <- control:
		return nil
	}
}
func (r *Run) Snapshot() workflow.State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return ownedState(r.state)
}
func (r *Run) Observation() *Observation {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.observation == nil {
		return nil
	}
	o := ownedObservation(*r.observation)
	return &o
}
func (r *Run) Wait(ctx context.Context) (RunResult, error) {
	select {
	case <-ctx.Done():
		return RunResult{}, ctx.Err()
	case <-r.done:
		r.mu.RLock()
		defer r.mu.RUnlock()
		out := r.result
		out.Initial = ownedState(out.Initial)
		out.State = ownedState(out.State)
		out.Events = append([]workflow.Event(nil), out.Events...)
		for i := range out.Events {
			out.Events[i].Observation = out.Events[i].Observation.Clone()
		}
		return out, r.err
	}
}
func (r *Run) Done() <-chan struct{} { return r.done }
func ownedState(s workflow.State) workflow.State {
	s.Observation = s.Observation.Clone()
	m := make(map[string]workflow.Value, len(s.Outputs))
	for k, v := range s.Outputs {
		m[k] = v
	}
	s.Outputs = m
	return s
}
func ownedObservation(o Observation) Observation {
	nodes := make([]Node, len(o.Nodes))
	for i, n := range o.Nodes {
		nodes[i] = n
		nodes[i].Actions = append([]string(nil), n.Actions...)
		nodes[i].NativePath = append([]uint32(nil), n.NativePath...)
		if n.Value != nil {
			v := *n.Value
			nodes[i].Value = &v
		}
		if n.Parent != nil {
			v := *n.Parent
			nodes[i].Parent = &v
		}
		if n.Identifier != nil {
			v := *n.Identifier
			nodes[i].Identifier = &v
		}
		if n.Bounds != nil {
			v := *n.Bounds
			nodes[i].Bounds = &v
		}
	}
	o.Nodes = nodes
	return o
}

type workResult struct {
	epoch               uint64
	command             workflow.Command
	observation         *Observation
	observationRecorded bool
	event               workflow.Event
	err                 error
	resume              bool
	human               bool
	actor               string
	receipt             *Receipt
	fingerprint         [32]byte
}

func (r *Run) execute(ctx context.Context, opts RunOptions, initial workflow.State) {
	s := initial
	session := opts.Session
	events := []workflow.Event{}
	results := make(chan workResult, 2)
	var executionErr error
	var cancelWork context.CancelFunc
	var pending bool
	var inputPending bool
	var assistedUsed bool
	var reviewed [32]byte
	var apply func(workflow.Event) ([]workflow.Command, error)
	lastClock := time.Now()
	interventionStart := time.Time{}
	var elapsed time.Duration
	onRecord := opts.OnRecord
	var recordMu sync.Mutex
	record := func(rec Record) error {
		if onRecord == nil {
			return nil
		}
		rec = ownedRecord(rec)
		rec.Reason = opts.Privacy.Scrub(rec.Reason)
		recordMu.Lock()
		defer recordMu.Unlock()
		return onRecord(rec)
	}
	opts.OnRecord = record
	publish := func() { r.mu.Lock(); r.state = ownedState(s); r.mu.Unlock() }
	defer func() {
		if cancelWork != nil {
			cancelWork()
		}
		if executionErr != nil || s.Phase != workflow.Completed {
			fenceCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			updated, fenceErr := opts.Desktop.Control(fenceCtx, session, "pause")
			cancel()
			if fenceErr == nil {
				session = updated
				_, fenceErr = apply(workflow.Event{Kind: "fence", NextEpoch: updated.Epoch, Reason: "terminal input fence"})
				// Even a failed journal must expose the actual broker epoch so
				// the caller can detach; failure remains explicit in RunResult.
				s.Epoch = updated.Epoch
			}
			executionErr = errors.Join(executionErr, fenceErr)
		}
		if executionErr != nil && !s.Terminal() {
			if inputPending || s.Phase == workflow.Acting || s.Phase == workflow.PostAction {
				s.Phase = workflow.Unknown
			} else {
				s.Phase = workflow.Failed
			}
			s.Reason = opts.Privacy.Scrub(executionErr.Error())
		}
		r.mu.Lock()
		r.state = ownedState(s)
		r.result = RunResult{Initial: initial, State: ownedState(s), Events: events}
		if executionErr != nil {
			r.result.Error = opts.Privacy.Scrub(executionErr.Error())
		}
		r.err = executionErr
		r.mu.Unlock()
		close(r.done)
	}()
	clock := func() {
		now := time.Now()
		if s.Phase != workflow.Paused && s.Phase != workflow.AwaitingApproval {
			elapsed += now.Sub(lastClock)
		}
		lastClock = now
	}
	interruption := func(kind, reason string) workflow.Event {
		e := workflow.Event{Kind: kind, Reason: reason}
		if inputPending {
			e.Delivery = "unknown"
		}
		return e
	}
	apply = func(event workflow.Event) ([]workflow.Command, error) {
		clock()
		event.Sequence = s.Sequence + 1
		event.RunID = s.RunID
		event.SessionID = s.SessionID
		event.Epoch = s.Epoch
		event.ElapsedMillis = elapsed.Milliseconds()
		safe := event
		safe.Reason = opts.Privacy.Scrub(safe.Reason)
		safe.Observation.Reason = opts.Privacy.Scrub(safe.Observation.Reason)
		if safe.Observation.Value.Type == "string" {
			safe.Observation.Value.Text = opts.Privacy.Scrub(safe.Observation.Value.Text)
		}
		next, commands, err := workflow.Reduce(opts.Program, s, safe, opts.Inputs)
		if err != nil {
			return nil, err
		}
		if err := record(Record{Kind: "event", ActionID: event.ActionID, Actor: "automation", Epoch: s.Epoch, Event: &safe}); err != nil {
			return nil, err
		}
		s = next
		events = append(events, safe)
		if s.Phase == workflow.Paused || s.Phase == workflow.AwaitingApproval {
			if interventionStart.IsZero() {
				interventionStart = time.Now()
			}
		} else {
			interventionStart = time.Time{}
		}
		publish()
		return commands, nil
	}
	launch := func(command workflow.Command) {
		if command.Kind == "request_approval" {
			return
		}
		if pending {
			executionErr = errors.New("execution attempted overlapping native requests")
			return
		}
		if command.Kind == "act" || (command.Kind == "observe" && (command.Step.Kind == "extract" || command.Step.Kind == "assert" || command.Step.Kind == "branch" || command.Step.Kind == "wait")) {
			if err := record(Record{Kind: "action_started", Stage: command.Kind, StepKind: command.Step.Kind, ActionID: command.ActionID, Actor: "automation", Epoch: s.Epoch}); err != nil {
				executionErr = err
				return
			}
		}
		pending = true
		inputPending = command.Kind == "act"
		workCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		cancelWork = cancel
		capturedSession := session
		outputs := ownedState(s).Outputs
		review := reviewed
		go func() {
			result := perform(workCtx, opts, capturedSession, outputs, command, review)
			select {
			case results <- result:
			case <-ctx.Done():
			case <-r.done:
			}
		}()
	}
	commands, err := apply(workflow.Event{Kind: "start"})
	if err != nil {
		executionErr = err
		return
	}
	for _, cmd := range commands {
		launch(cmd)
	}
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for !s.Terminal() && executionErr == nil {
		select {
		case <-ctx.Done():
			_, reduceErr := apply(interruption("cancel", "caller cancelled"))
			executionErr = errors.Join(ctx.Err(), reduceErr)
		case <-ticker.C:
			clock()
			if elapsed > time.Duration(opts.Program.Capability().Limits.ActiveSeconds)*time.Second {
				if cancelWork != nil {
					cancelWork()
				}
				_, err := apply(interruption("cancel", "active deadline"))
				executionErr = errors.Join(errors.New("active execution deadline exceeded"), err)
			}
			if !interventionStart.IsZero() && time.Since(interventionStart) > time.Duration(opts.Program.Capability().Limits.InterventionSeconds)*time.Second {
				_, err := apply(interruption("cancel", "intervention deadline"))
				executionErr = errors.Join(errors.New("intervention deadline exceeded"), err)
			}
		case result := <-results:
			if result.epoch != s.Epoch {
				continue
			}
			pending = false
			inputPending = result.human && result.receipt != nil && result.receipt.Delivery != "not_sent"
			if cancelWork != nil {
				cancelWork()
				cancelWork = nil
			}
			if result.human && result.receipt != nil {
				if err := record(Record{Kind: "human_action", ActionID: result.receipt.ActionID, Actor: result.actor, Epoch: s.Epoch, Receipt: result.receipt}); err != nil {
					executionErr = err
					continue
				}
			}
			if result.observation != nil {
				reviewed = result.fingerprint
				safe := ownedObservation(*result.observation)
				r.mu.Lock()
				r.observation = &safe
				r.mu.Unlock()
				actor := "automation"
				if result.human {
					actor = result.actor
					if actor == "" {
						actor = "human"
					}
				}
				if !result.observationRecorded {
					if err := record(Record{Kind: "observation", Stage: result.command.Kind, StepKind: result.command.Step.Kind, ActionID: result.command.ActionID, Actor: actor, Epoch: s.Epoch, Observation: &safe}); err != nil {
						executionErr = err
						continue
					}
				}
			}
			if result.resume {
				if result.err != nil {
					if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: result.err.Error()}); err != nil {
						executionErr = err
					}
					continue
				}
				resumeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				updated, err := opts.Desktop.Resume(resumeCtx, session, result.observation.ID)
				cancel()
				if err != nil {
					executionErr = err
					continue
				}
				session = updated
				commands, err := apply(workflow.Event{Kind: "resume", NextEpoch: updated.Epoch})
				if err != nil {
					executionErr = err
					continue
				}
				for _, cmd := range commands {
					launch(cmd)
				}
				continue
			}
			if result.human {
				if result.err != nil {
					if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Actor: result.actor, Reason: result.err.Error()}); err != nil {
						executionErr = err
					}
				}
				if executionErr == nil && result.receipt != nil && (result.receipt.Delivery == "unknown" || (result.receipt.Delivery == "sent" && result.err != nil)) {
					_, executionErr = apply(workflow.Event{Kind: "human_unknown", ActionID: result.receipt.ActionID, Reason: "human action requires outcome reconciliation"})
				}
				if executionErr == nil {
					inputPending = false
				}
				continue
			}
			if result.err != nil {
				var changed *approvalChanged
				if errors.As(result.err, &changed) {
					commands, err := apply(workflow.Event{Kind: "approval_changed", ActionID: s.ActionID(opts.Program), Reason: "application state changed during approval; review the fresh observation"})
					if err != nil {
						executionErr = err
						continue
					}
					for _, cmd := range commands {
						launch(cmd)
					}
					continue
				}
				result.event = workflow.Event{Kind: "error", Reason: result.err.Error()}
				var captureError *BrokerError
				if (result.command.Kind == "observe" || result.command.Kind == "observe_after") && errors.As(result.err, &captureError) && captureError.Delivery == "not_sent" && (captureError.Code == "observation_inconsistent" || captureError.Code == "window_changed" || captureError.Code == "state_changed" || captureError.Code == "window_ambiguous" || captureError.Code == "window_occluded") {
					result.event = workflow.Event{Kind: "observe_failed", ActionID: result.command.ActionID, FailureCode: captureError.Code, Reason: captureError.Error()}
				}
				if (result.command.Kind == "observe" || result.command.Kind == "observe_after") && errors.As(result.err, &captureError) && captureError.Delivery == "not_sent" && captureError.Code == "external_interaction" {
					result.event = workflow.Event{Kind: "external_interaction", ActionID: result.command.ActionID, FailureCode: captureError.Code, Reason: captureError.Error()}
				}
				if s.Phase == workflow.Acting {
					delivery := "unknown"
					if result.receipt == nil {
						delivery = "not_sent"
					}
					var e *BrokerError
					if errors.As(result.err, &e) {
						delivery = e.Delivery
					}
					// A returned receipt indicating possible delivery outranks an
					// inconsistent error. Never let not_sent turn a delivered input
					// into an automatic retry.
					if result.receipt != nil && (result.receipt.Delivery == "sent" || result.receipt.Delivery == "unknown") {
						delivery = "unknown"
					}
					result.event = workflow.Event{Kind: "receipt", ActionID: s.ActionID(opts.Program), Delivery: delivery, Reason: result.err.Error()}
					if e != nil {
						result.event.FailureCode = e.Code
					}
				}
			}
			commands, err := apply(result.event)
			if err != nil {
				executionErr = err
				continue
			}
			if s.Phase == workflow.Paused {
				fenceCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				updated, fenceErr := opts.Desktop.Control(fenceCtx, session, "pause")
				cancel()
				if fenceErr != nil {
					executionErr = fenceErr
					continue
				}
				session = updated
				if _, fenceErr = apply(workflow.Event{Kind: "pause", NextEpoch: updated.Epoch, Reason: s.Reason}); fenceErr != nil {
					executionErr = fenceErr
					continue
				}
			}
			for _, cmd := range commands {
				launch(cmd)
			}
		case control := <-r.control:
			if control.Epoch != s.Epoch {
				if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: "stale control epoch"}); err != nil {
					executionErr = err
				}
				continue
			}
			switch control.Kind {
			case "approve", "deny":
				event := workflow.Event{Kind: control.Kind, ActionID: control.ActionID, Observation: workflow.Observation{ID: control.ObservationID}}
				commands, err := apply(event)
				if err != nil {
					if logErr := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: err.Error()}); logErr != nil {
						executionErr = logErr
					}
					continue
				}
				for _, cmd := range commands {
					launch(cmd)
				}
			case "pause":
				if s.Phase == workflow.Paused && !inputPending {
					continue
				}
				fenceCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				updated, err := opts.Desktop.Control(fenceCtx, session, "pause")
				cancel()
				if err != nil {
					executionErr = err
					continue
				}
				if cancelWork != nil {
					cancelWork()
					cancelWork = nil
				}
				pending = false
				session = updated
				e := interruption("pause", "human takeover")
				e.NextEpoch = updated.Epoch
				_, err = apply(e)
				if err != nil {
					executionErr = err
				}
			case "resume", "refresh", "focus":
				if control.Kind == "focus" {
					if _, ok := opts.Desktop.(Focuser); !ok {
						if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: "desktop does not support focus"}); err != nil {
							executionErr = err
						}
						continue
					}
				}
				if s.Phase != workflow.Paused || pending {
					if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: "resume/refresh requires idle paused run"}); err != nil {
						executionErr = err
					}
					continue
				}
				if control.Kind == "resume" {
					if err := s.ValidateResume(opts.Program); err != nil {
						if logErr := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: err.Error()}); logErr != nil {
							executionErr = logErr
						}
						continue
					}
				}
				pending = true
				workCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				cancelWork = cancel
				capturedSession := session
				resume := control.Kind == "resume"
				go func() {
					if f, ok := opts.Desktop.(Focuser); ok {
						_, err := f.Focus(workCtx, capturedSession)
						if err != nil {
							select {
							case results <- workResult{epoch: capturedSession.Epoch, err: err, resume: resume, human: !resume}:
							case <-ctx.Done():
							}
							return
						}
					}
					raw, err := opts.Desktop.Observe(workCtx, capturedSession)
					var safe Observation
					if err == nil {
						safe, err = Sanitize(raw, opts.Privacy)
					}
					result := workResult{epoch: capturedSession.Epoch, err: err, resume: resume, human: !resume, command: workflow.Command{Kind: "refresh"}, fingerprint: semanticFingerprint(raw)}
					if err == nil {
						result.observation = &safe
					}
					select {
					case results <- result:
					case <-ctx.Done():
					case <-r.done:
					}
				}()
			case "human_act", "assisted_action":
				if s.Phase != workflow.Paused || pending {
					if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: "human action requires idle paused run"}); err != nil {
						executionErr = err
					}
					continue
				}
				actor := "human"
				var recoverySelector Selector
				if control.Kind == "assisted_action" {
					var err error
					recoverySelector, err = validateRecovery(control, r.Observation(), opts.Assisted, assistedUsed)
					if err != nil {
						if recordErr := record(Record{Kind: "control_refused", Epoch: s.Epoch, Actor: "model_recovery", Reason: err.Error()}); recordErr != nil {
							executionErr = recordErr
						}
						continue
					}
					assistedUsed = true
					actor = "model_recovery"
				}
				pending = true
				inputPending = true
				workCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
				cancelWork = cancel
				capturedSession := session
				a := control.Action
				if err := record(Record{Kind: "action_started", Stage: control.Kind, StepKind: a.Kind, ActionID: a.ActionID, Actor: actor, Epoch: s.Epoch}); err != nil {
					executionErr = err
					continue
				}
				go func() {
					var err error
					if actor == "model_recovery" {
						node, resolveErr := opts.Desktop.Resolve(workCtx, capturedSession, a.ObservationID, recoverySelector)
						err = resolveErr
						if err == nil && (node.ID != a.TargetID || node.Role != recoverySelector.Role || node.Name != recoverySelector.Name || !node.Enabled) {
							err = errors.New("recovery target changed")
						}
					}

					if a.Effect == "change" {
						a.ApprovalID, err = opts.Desktop.Approve(workCtx, capturedSession, a)
					}
					receipt := Receipt{ActionID: a.ActionID, Delivery: "not_sent"}
					if err == nil {
						receipt, err = opts.Desktop.HumanAct(workCtx, capturedSession, a)
						if err != nil {
							receipt = Receipt{ActionID: a.ActionID, Delivery: "unknown"}
							var e *BrokerError
							if errors.As(err, &e) && e.Delivery == "not_sent" {
								receipt.Delivery = "not_sent"
							}
						}
					}
					result := workResult{epoch: capturedSession.Epoch, human: true, actor: actor, err: err, receipt: &receipt, command: workflow.Command{Kind: "human_after", ActionID: a.ActionID, Step: workflow.Step{Kind: a.Kind}}}
					if err == nil && receipt.Delivery == "sent" {
						raw, observeErr := opts.Desktop.Observe(workCtx, capturedSession)
						if observeErr == nil {
							safe, safeErr := Sanitize(raw, opts.Privacy)
							if safeErr == nil {
								result.observation = &safe
								result.fingerprint = semanticFingerprint(raw)
							} else {
								observeErr = safeErr
							}
						}
						if observeErr != nil {
							result.err = observeErr
						}
					}
					select {
					case results <- result:
					case <-ctx.Done():
					case <-r.done:
					}
				}()
			case "cancel":
				_, executionErr = apply(interruption("cancel", "cancelled"))
			default:
				if err := record(Record{Kind: "control_refused", Epoch: s.Epoch, Reason: "unsupported control"}); err != nil {
					executionErr = err
				}
			}
		}
	}
	if executionErr != nil && !s.Terminal() {
		_, err := apply(interruption("error", executionErr.Error()))
		executionErr = errors.Join(executionErr, err)
	}
}

func perform(ctx context.Context, opts RunOptions, session Session, outputs map[string]workflow.Value, command workflow.Command, reviewed [32]byte) workResult {
	r := workResult{epoch: session.Epoch, command: command}
	if command.DelayMillis > 0 {
		timer := time.NewTimer(time.Duration(command.DelayMillis) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			r.err = ctx.Err()
			return r
		case <-timer.C:
		}
	}
	if command.Kind == "act" {
		a := Action{ActionID: command.ActionID, ObservationID: command.ObservationID, TargetID: command.TargetID, Kind: command.Step.Kind, Effect: command.Step.Effect}
		if a.Kind == "set_value" || a.Kind == "type_text" || a.Kind == "scroll" {
			v, err := workflow.Resolve(command.Step.Input, opts.Inputs, outputs)
			if err != nil {
				r.err = err
				return r
			}
			if a.Kind == "scroll" {
				if v.Type != "integer" || v.Integer < -20 || v.Integer > 20 || v.Integer == 0 {
					r.err = errors.New("invalid scroll input")
					return r
				}
				n := int32(v.Integer)
				a.ScrollY = &n
			} else {
				text := v.Display()
				a.Text = &text
			}
		}
		if a.Effect == "change" {
			f, ok := opts.Desktop.(Focuser)
			if !ok {
				r.err = errors.New("desktop cannot focus for approval revalidation")
				return r
			}
			if _, err := f.Focus(ctx, session); err != nil {
				r.err = err
				return r
			}
			raw, err := opts.Desktop.Observe(ctx, session)
			if err != nil {
				r.err = err
				return r
			}
			safe, err := Sanitize(raw, opts.Privacy)
			if err != nil {
				r.err = err
				return r
			}
			r.observation = &safe
			r.fingerprint, err = approvalFingerprint(raw, command.Selector)
			if err != nil {
				r.err = err
				return r
			}
			if r.fingerprint != reviewed {
				r.err = &approvalChanged{}
				return r
			}
			selector := selectorFrom(command.Selector)
			if err := validateVisualPrivacy(raw, opts.Privacy, selector.Visual); err != nil {
				r.err = err
				return r
			}
			target, err := resolveTarget(ctx, opts.Desktop, session, raw.ID, selector)
			if err != nil {
				r.err = err
				return r
			}
			a.ObservationID = raw.ID
			if target.Visual != nil {
				a.TargetID = target.Visual.TargetID
			} else {
				a.TargetID = target.Node.ID
			}
			if err := opts.OnRecord(Record{Kind: "observation", Stage: "approval_revalidation", StepKind: command.Step.Kind, ActionID: command.ActionID, Actor: "automation", Epoch: session.Epoch, Observation: &safe}); err != nil {
				r.err = err
				return r
			}
			r.observationRecorded = true
			approval, err := opts.Desktop.Approve(ctx, session, a)
			if err != nil {
				r.err = err
				return r
			}
			a.ApprovalID = approval
		}
		receipt, err := opts.Desktop.Act(ctx, session, a)
		r.err = err
		r.receipt = &receipt
		r.event = workflow.Event{Kind: "receipt", ActionID: command.ActionID, Delivery: receipt.Delivery}
		return r
	}
	if command.Refocus {
		f, ok := opts.Desktop.(Focuser)
		if !ok {
			r.err = errors.New("desktop cannot focus for pre-dispatch recovery")
			return r
		}
		if _, err := f.Focus(ctx, session); err != nil {
			r.err = err
			return r
		}
	}
	raw, err := opts.Desktop.Observe(ctx, session)
	if err != nil {
		r.err = err
		return r
	}
	safe, err := Sanitize(raw, opts.Privacy)
	if err != nil {
		r.err = err
		return r
	}
	r.observation = &safe
	r.fingerprint, err = approvalFingerprint(raw, command.Selector)
	if err != nil {
		r.err = err
		return r
	}
	o := workflow.Observation{ID: raw.ID, Complete: raw.Complete, Reason: raw.TruncatedReason}
	if command.Kind != "observe_after" {
		selector := selectorFrom(command.Selector)
		if err := validateVisualPrivacy(raw, opts.Privacy, selector.Visual); err != nil {
			r.err = err
			return r
		}
		target, err := resolveTarget(ctx, opts.Desktop, session, raw.ID, selector)
		if err != nil {
			var brokerErr *BrokerError
			if errors.As(err, &brokerErr) && (brokerErr.Code == "target_missing" || brokerErr.Code == "target_ambiguous") {
				if brokerErr.Code == "target_ambiguous" {
					o.Matches = 2
				}
				o.Reason = brokerErr.Error()
			} else {
				r.err = err
				return r
			}
		} else {
			o.Matches = 1
			if target.Visual != nil {
				visual, err := publicVisualMatch(safe, *target.Visual)
				if err != nil {
					r.err = err
					return r
				}
				o.Visual = &visual
				o.TargetID = target.Visual.TargetID
				o.Actionable = true
				r.event = workflow.Event{Kind: "observed", ActionID: command.ActionID, Observation: o}
				return r
			}
			node := *target.Node
			o.TargetID = node.ID
			o.Actionable = node.Enabled
			// Derive every exported typed value from the sanitized tree. Merely
			// scrubbing the serialized string event misses numeric projections.
			var visible *Node
			for i := range safe.Nodes {
				if safe.Nodes[i].ID == node.ID {
					visible = &safe.Nodes[i]
					break
				}
			}
			if visible == nil {
				r.err = errors.New("resolved target missing from sanitized observation")
				return r
			}
			text := visible.Name
			if visible.Value != nil {
				text = *visible.Value
			}
			typ := "string"
			currency := ""
			if command.Step.Kind == "extract" {
				typ = command.Step.OutputType
				currency = command.Step.Currency
			} else if command.Step.Kind == "assert" || command.Step.Kind == "branch" || command.Step.Kind == "wait" {
				if command.Step.Predicate.Op != "exists" {
					expected, e := workflow.Resolve(command.Step.Predicate.Expected, opts.Inputs, outputs)
					if e != nil {
						r.err = e
						return r
					}
					typ = expected.Type
					currency = expected.Currency
				}
			}
			o.Value, err = ParseValue(text, typ, currency)
			if err != nil {
				r.err = err
				return r
			}
		}
	}
	r.event = workflow.Event{Kind: "observed", ActionID: command.ActionID, Observation: o}
	return r
}

func ParseValue(text, typ, currency string) (workflow.Value, error) {
	s := strings.TrimSpace(text)
	v := workflow.Value{Type: typ, Currency: currency}
	switch typ {
	case "string":
		v.Text = text
	case "integer":
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return v, errors.New("observed integer is invalid")
		}
		v.Integer = n
	case "boolean":
		b, err := strconv.ParseBool(s)
		if err != nil {
			return v, errors.New("observed boolean is invalid")
		}
		v.Boolean = b
	case "money":
		n, err := parseMinorUnits(s, currency)
		if err != nil {
			return v, err
		}
		v.Integer = n
	default:
		return v, fmt.Errorf("unsupported output type %q", typ)
	}
	return v, v.Validate()
}

func parseMinorUnits(s, currency string) (int64, error) {
	if currency != "" && strings.HasSuffix(s, " "+currency) {
		s = strings.TrimSpace(strings.TrimSuffix(s, " "+currency))
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if strings.HasPrefix(s, "$") {
		if currency != "USD" {
			return 0, errors.New("dollar symbol requires USD")
		}
		s = strings.TrimPrefix(s, "$")
	}
	if !negative && strings.HasPrefix(s, "-") {
		negative = true
		s = strings.TrimPrefix(s, "-")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("invalid money")
	}
	groups := strings.Split(parts[0], ",")
	digits := func(s string) bool {
		if s == "" {
			return false
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
		}
		return true
	}
	for i, g := range groups {
		if !digits(g) || (len(groups) > 1 && ((i == 0 && len(g) > 3) || (i > 0 && len(g) != 3))) {
			return 0, errors.New("invalid grouped money")
		}
	}
	whole, err := strconv.ParseUint(strings.Join(groups, ""), 10, 64)
	if err != nil {
		return 0, errors.New("money outside bounds")
	}
	var fraction uint64
	if len(parts) == 2 {
		if len(parts[1]) != 2 || !digits(parts[1]) {
			return 0, errors.New("money requires two fractional digits")
		}
		fraction = uint64(parts[1][0]-'0')*10 + uint64(parts[1][1]-'0')
	}
	limit := uint64(math.MaxInt64)
	if negative {
		limit++
	}
	if whole > limit/100 || whole*100 > limit-fraction {
		return 0, errors.New("money outside bounds")
	}
	minor := whole*100 + fraction
	if negative {
		if minor == uint64(math.MaxInt64)+1 {
			return math.MinInt64, nil
		}
		return -int64(minor), nil
	}
	return int64(minor), nil
}
