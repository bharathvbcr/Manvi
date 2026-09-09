package workflow

import (
	"errors"
	"fmt"
)

type Phase string

const (
	Ready            Phase = "ready"
	Observing        Phase = "observing"
	AwaitingApproval Phase = "awaiting_approval"
	Acting           Phase = "acting"
	PostAction       Phase = "post_action"
	Paused           Phase = "paused"
	Completed        Phase = "completed"
	Failed           Phase = "failed"
	Cancelled        Phase = "cancelled"
	Unknown          Phase = "outcome_unknown"
)

type Observation struct {
	Visual     *VisualMatch `json:"visual,omitempty"`
	ID         string       `json:"id"`
	TargetID   string       `json:"target_id"`
	Matches    int          `json:"matches"`
	Complete   bool         `json:"complete"`
	Actionable bool         `json:"actionable"`
	Value      Value        `json:"value"`
	Reason     string       `json:"reason,omitempty"`
}

func (o Observation) Clone() Observation {
	if o.Visual != nil {
		v := *o.Visual
		o.Visual = &v
	}
	return o
}

type Event struct {
	Sequence      uint64      `json:"sequence"`
	RunID         string      `json:"run_id"`
	SessionID     string      `json:"session_id"`
	Epoch         uint64      `json:"epoch"`
	NextEpoch     uint64      `json:"next_epoch,omitempty"`
	ElapsedMillis int64       `json:"elapsed_millis"`
	Kind          string      `json:"kind"`
	ActionID      string      `json:"action_id,omitempty"`
	Observation   Observation `json:"observation"`
	Delivery      string      `json:"delivery,omitempty"`
	FailureCode   string      `json:"failure_code,omitempty"`
	Reason        string      `json:"reason,omitempty"`
}

type State struct {
	RunID            string `json:"run_id"`
	SessionID        string `json:"session_id"`
	Epoch            uint64 `json:"epoch"`
	CapabilitySHA256 string `json:"capability_sha256"`
	Sequence         uint64 `json:"sequence"`
	ElapsedMillis    int64  `json:"elapsed_millis"`
	Phase            Phase  `json:"phase"`
	// ResumePhase prevents replaying an action whose receipt was already sent.
	ResumePhase Phase `json:"resume_phase,omitempty"`
	StepIndex   int   `json:"step_index"`
	Actions     int   `json:"actions"`
	Attempts    int   `json:"attempts"`
	// ActionAttempt advances only after explicit recoverable not_sent input.
	// It stays monotonic across pauses so a native deduplication ID is never reused.
	ActionAttempt int              `json:"action_attempt,omitempty"`
	Observation   Observation      `json:"observation"`
	Outputs       map[string]Value `json:"outputs"`
	Reason        string           `json:"reason,omitempty"`
}

type Command struct {
	Refocus       bool     `json:"refocus,omitempty"`
	DelayMillis   int64    `json:"delay_millis,omitempty"`
	Kind          string   `json:"kind"`
	ActionID      string   `json:"action_id"`
	Step          Step     `json:"step"`
	Selector      Selector `json:"selector"`
	ObservationID string   `json:"observation_id,omitempty"`
	TargetID      string   `json:"target_id,omitempty"`
	Epoch         uint64   `json:"epoch"`
}

func NewState(p *Program, runID, sessionID string, epoch uint64) (State, error) {
	if p == nil || runID == "" || sessionID == "" || epoch == 0 {
		return State{}, errors.New("program, run, session and positive epoch required")
	}
	return State{RunID: runID, SessionID: sessionID, Epoch: epoch, CapabilitySHA256: p.Digest(), Phase: Ready, Outputs: map[string]Value{}}, nil
}
func (s State) Terminal() bool {
	return s.Phase == Completed || s.Phase == Failed || s.Phase == Cancelled || s.Phase == Unknown
}

// ValidateResume checks policy before the host changes the native session epoch.
func (s State) ValidateResume(p *Program) error {
	if p == nil || s.CapabilitySHA256 != p.Digest() {
		return errors.New("program identity mismatch")
	}
	if s.Phase != Paused {
		return errors.New("resume requires paused phase")
	}
	if s.ActionAttempt >= p.spec.Limits.ObservationAttempts {
		return errors.New("pre-dispatch action attempt budget exhausted; start a new run after reconciliation")
	}
	return nil
}
func (s State) ActionID(p *Program) string {
	if s.StepIndex < 0 || s.StepIndex >= len(p.spec.Steps) {
		return ""
	}
	id := s.RunID + ":" + p.spec.Steps[s.StepIndex].ID
	if s.ActionAttempt > 0 {
		id += fmt.Sprintf(":attempt:%d", s.ActionAttempt+1)
	}
	return id
}
func copyState(s State) State {
	s.Observation = s.Observation.Clone()
	m := make(map[string]Value, len(s.Outputs))
	for k, v := range s.Outputs {
		m[k] = v
	}
	s.Outputs = m
	return s
}

// Reduce is pure. A rejected envelope leaves the caller's state untouched.
// Clock advances, observations and human decisions must arrive as events.
func Reduce(p *Program, previous State, event Event, inputs map[string]Value) (State, []Command, error) {
	event.Observation = event.Observation.Clone()
	if p == nil || previous.CapabilitySHA256 != p.Digest() {
		return previous, nil, errors.New("program identity mismatch")
	}
	if previous.ActionAttempt < 0 || previous.ActionAttempt > p.spec.Limits.ObservationAttempts {
		return previous, nil, errors.New("invalid action attempt state")
	}
	if event.RunID != previous.RunID || event.SessionID != previous.SessionID || event.Epoch != previous.Epoch {
		return previous, nil, errors.New("stale or foreign event context")
	}
	if event.Sequence == 0 || event.Sequence != previous.Sequence+1 || event.ElapsedMillis < previous.ElapsedMillis {
		return previous, nil, errors.New("event sequence or clock regressed")
	}
	if event.Kind == "fence" {
		if event.NextEpoch <= previous.Epoch {
			return previous, nil, errors.New("fence requires a newer broker epoch")
		}
		next := copyState(previous)
		next.Epoch = event.NextEpoch
		next.Sequence = event.Sequence
		next.ElapsedMillis = event.ElapsedMillis
		return next, nil, nil
	}
	if previous.Terminal() {
		return previous, nil, errors.New("run is terminal")
	}
	if previous.StepIndex < 0 || previous.StepIndex >= len(p.spec.Steps) {
		return previous, nil, errors.New("invalid step state")
	}
	s := copyState(previous)
	s.Sequence = event.Sequence
	s.ElapsedMillis = event.ElapsedMillis
	if s.ElapsedMillis > int64(p.spec.Limits.ActiveSeconds)*1000 {
		if s.Phase == Acting {
			s.Phase = Unknown
		} else {
			s.Phase = Failed
		}
		s.Reason = "active execution deadline exceeded"
		return s, nil, nil
	}
	if event.Kind == "cancel" {
		if s.Phase == Acting || event.Delivery == "unknown" {
			s.Phase = Unknown
			s.Reason = "cancelled while action delivery was unresolved"
		} else {
			s.Phase = Cancelled
			s.Reason = "cancelled"
		}
		return s, nil, nil
	}
	if event.Kind == "pause" {
		if event.NextEpoch <= s.Epoch {
			return previous, nil, errors.New("pause requires a broker-fenced newer epoch")
		}
		s.Epoch = event.NextEpoch
		s.Observation = Observation{}
		if s.Phase == PostAction {
			s.ResumePhase = PostAction
		} else if s.Phase != Paused {
			s.ResumePhase = Observing
		}
		if s.Phase == Acting || event.Delivery == "unknown" {
			s.Phase = Unknown
			s.Reason = "takeover after possible dispatch requires outcome reconciliation"
		} else {
			s.Phase = Paused
			s.Reason = event.Reason
		}
		return s, nil, nil
	}
	step := p.spec.Steps[s.StepIndex]
	command := func(kind string) Command {
		return Command{Kind: kind, Refocus: kind == "observe" && s.ActionAttempt > 0, ActionID: s.ActionID(p), Step: step, Selector: p.spec.Targets[step.Target].Clone(), ObservationID: s.Observation.ID, TargetID: s.Observation.TargetID, Epoch: s.Epoch}
	}
	switch event.Kind {
	case "start":
		if s.Phase != Ready {
			return previous, nil, errors.New("start requires ready phase")
		}
		if err := p.ValidateInputs(inputs); err != nil {
			return previous, nil, err
		}
		s.Phase = Observing
		s.Actions = 1
		return s, []Command{command("observe")}, nil
	case "resume":
		if err := s.ValidateResume(p); err != nil {
			return previous, nil, err
		}
		if event.NextEpoch <= s.Epoch {
			return previous, nil, errors.New("resume epoch regressed")
		}
		if event.NextEpoch > s.Epoch {
			s.Epoch = event.NextEpoch
		}
		s.Phase = Observing
		kind := "observe"
		if s.ResumePhase == PostAction {
			s.Phase = PostAction
			kind = "observe_after"
		}
		s.ResumePhase = ""
		s.Attempts = 0
		s.Reason = ""
		return s, []Command{command(kind)}, nil
	case "observe_failed":
		if (s.Phase != Observing && s.Phase != PostAction) || event.ActionID != s.ActionID(p) {
			return previous, nil, errors.New("capture failure does not match an observation command")
		}
		return retryObservation(p, s, command("observe"), event.Reason)
	case "observed":
		if s.Phase != Observing && s.Phase != PostAction {
			return previous, nil, errors.New("unsolicited observation")
		}
		if event.ActionID != s.ActionID(p) || event.Observation.ID == "" {
			return previous, nil, errors.New("observation action binding missing")
		}
		if !event.Observation.Complete {
			return retryObservation(p, s, command("observe"), "incomplete observation: "+event.Observation.Reason)
		}
		if s.Phase == PostAction {
			return advance(p, s, step.Next)
		}
		if event.Observation.Matches != 1 || event.Observation.TargetID == "" {
			return retryObservation(p, s, command("observe"), fmt.Sprintf("target resolves to %d elements", event.Observation.Matches))
		}
		s.Observation = event.Observation
		if anchor := p.spec.Targets[step.Target].Visual; anchor != nil {
			v := event.Observation.Visual
			if v == nil || v.TargetID != event.Observation.TargetID || v.AnchorSHA256 != anchor.SHA256 || len(v.FrameSHA256) != 64 || !v.Matched.Inside(anchor.FrameWidth, anchor.FrameHeight) || v.Matched.Width != anchor.Width || v.Matched.Height != anchor.Height {
				return previous, nil, errors.New("visual observation lacks its native match binding")
			}
		} else if event.Observation.Visual != nil {
			return previous, nil, errors.New("semantic observation contains a visual match")
		}
		if step.Kind != "wait" {
			s.Attempts = 0
		}
		switch step.Kind {
		case "extract":
			if event.Observation.Value.Type != step.OutputType || event.Observation.Value.Validate() != nil {
				return fail(s, "observed output has wrong type")
			}
			if step.OutputType == "money" && event.Observation.Value.Currency != step.Currency {
				return fail(s, "observed currency differs")
			}
			s.Outputs[step.Output] = event.Observation.Value
			return advance(p, s, step.Next)
		case "assert", "branch", "wait":
			var expected Value
			if step.Predicate.Op != "exists" {
				var err error
				expected, err = Resolve(step.Predicate.Expected, inputs, s.Outputs)
				if err != nil {
					return fail(s, err.Error())
				}
			}
			passed := Compare(event.Observation.Value, step.Predicate.Op, expected)
			if step.Kind == "branch" {
				if passed {
					return advance(p, s, step.Next)
				}
				return advance(p, s, step.Otherwise)
			}
			if !passed {
				if step.Kind == "wait" {
					return retryObservation(p, s, command("observe"), "wait predicate not yet satisfied")
				}
				return fail(s, "checkpoint predicate failed")
			}
			return advance(p, s, step.Next)
		default:
			if !event.Observation.Actionable {
				s.Phase = Paused
				s.Reason = "target is not actionable: " + event.Observation.Reason
				return s, nil, nil
			}
			if step.Effect == "change" {
				s.Phase = AwaitingApproval
				return s, []Command{command("request_approval")}, nil
			}
			s.Phase = Acting
			return s, []Command{command("act")}, nil
		}
	case "approve":
		if s.Phase != AwaitingApproval || event.ActionID != s.ActionID(p) || event.Observation.ID != s.Observation.ID {
			return previous, nil, errors.New("approval does not match pending action and observation")
		}
		s.Phase = Acting
		return s, []Command{command("act")}, nil
	case "deny":
		if s.Phase != AwaitingApproval || event.ActionID != s.ActionID(p) {
			return previous, nil, errors.New("denial does not match pending action")
		}
		s.Phase = Cancelled
		s.Reason = "human denied action"
		return s, nil, nil
	case "receipt":
		if s.Phase != Acting || event.ActionID != s.ActionID(p) {
			return previous, nil, errors.New("receipt does not match in-flight action")
		}
		switch event.Delivery {
		case "sent":
			s.Phase = PostAction
			cmd := command("observe_after")
			return s, []Command{cmd}, nil
		case "not_sent":
			if event.FailureCode == "external_interaction" {
				s.ActionAttempt++
				s.Actions++
				if s.Actions > p.spec.Limits.MaxActions {
					return fail(s, "action limit exceeded")
				}
				s.Phase = Paused
				s.ResumePhase = Observing
				s.Observation = Observation{}
				s.Attempts = 0
				s.Reason = "external interaction requires human reconciliation: " + event.Reason
				return s, nil, nil
			}
			if event.FailureCode == "window_changed" || event.FailureCode == "state_changed" {
				s.ActionAttempt++
				s.Observation = Observation{}
				s.Attempts = 0
				s.Reason = "pre-dispatch application change: " + event.Reason
				if s.ActionAttempt >= p.spec.Limits.ObservationAttempts {
					s.Phase = Paused
					s.ResumePhase = Observing
					s.Reason = "pre-dispatch action attempt budget exhausted: " + event.Reason
					return s, nil, nil
				}
				s.Actions++
				if s.Actions > p.spec.Limits.MaxActions {
					return fail(s, "action limit exceeded")
				}
				s.Phase = Observing
				cmd := command("observe")
				cmd.DelayMillis = 200
				return s, []Command{cmd}, nil
			}
			return fail(s, "action not dispatched: "+event.Reason)
		case "unknown":
			s.Phase = Unknown
			s.Reason = event.Reason
			return s, nil, nil
		default:
			return previous, nil, errors.New("unsupported action disposition")
		}
	case "approval_changed":
		if s.Phase != Acting || event.ActionID != s.ActionID(p) || step.Effect != "change" {
			return previous, nil, errors.New("approval revalidation does not match pending action")
		}
		s.Phase = Observing
		s.Observation = Observation{}
		s.Reason = event.Reason
		return s, []Command{command("observe")}, nil
	case "external_interaction":
		if s.Phase != Observing && s.Phase != PostAction {
			return previous, nil, errors.New("external interaction observation does not match active capture")
		}
		s.ResumePhase = s.Phase
		s.Phase = Paused
		s.Observation = Observation{}
		s.Reason = "external interaction requires human reconciliation: " + event.Reason
		return s, nil, nil
	case "human_unknown":
		if s.Phase != Paused {
			return previous, nil, errors.New("human outcome requires paused run")
		}
		s.Phase = Unknown
		s.Reason = event.Reason
		return s, nil, nil
	case "error":
		if s.Phase == Acting || s.Phase == PostAction || event.Delivery == "unknown" {
			s.Phase = Unknown
			s.Reason = event.Reason
			return s, nil, nil
		}
		return fail(s, event.Reason)
	default:
		return previous, nil, fmt.Errorf("unsupported event %q", event.Kind)
	}
}

func fail(s State, reason string) (State, []Command, error) {
	s.Phase = Failed
	s.Reason = reason
	return s, nil, nil
}
func retryObservation(p *Program, s State, cmd Command, reason string) (State, []Command, error) {
	s.Attempts++
	s.Reason = reason
	if s.Attempts >= p.spec.Limits.ObservationAttempts {
		if s.Phase == PostAction {
			s.ResumePhase = PostAction
		} else {
			s.ResumePhase = Observing
		}
		s.Phase = Paused
		return s, nil, nil
	}
	if s.Phase == PostAction {
		cmd.Kind = "observe_after"
	}
	cmd.DelayMillis = 200
	return s, []Command{cmd}, nil
}
func advance(p *Program, s State, next string) (State, []Command, error) {
	i := s.StepIndex + 1
	if next != "" {
		i = p.positions[next]
	}
	s.StepIndex = i
	s.Observation = Observation{}
	s.Attempts = 0
	s.ActionAttempt = 0
	s.Reason = ""
	if i == len(p.spec.Steps) {
		s.Phase = Completed
		return s, nil, nil
	}
	s.Actions++
	if s.Actions > p.spec.Limits.MaxActions {
		return fail(s, "action limit exceeded")
	}
	s.Phase = Observing
	step := p.spec.Steps[i]
	return s, []Command{{Kind: "observe", ActionID: s.ActionID(p), Step: step, Selector: p.spec.Targets[step.Target].Clone(), Epoch: s.Epoch}}, nil
}

// Replay reconstructs recorded transitions. It has no executor to dispatch the
// resulting commands, even when the original journal includes native actions.
func Replay(p *Program, initial State, events []Event, inputs map[string]Value) (State, error) {
	s := initial
	for _, e := range events {
		next, _, err := Reduce(p, s, e, inputs)
		if err != nil {
			return s, fmt.Errorf("event %d: %w", e.Sequence, err)
		}
		s = next
	}
	return s, nil
}
