package workflow

import (
	"fmt"
	"testing"
)

func mustNewState(t *testing.T, p *Program) State {
	t.Helper()
	s, err := NewState(p, "run", "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mustReduce applies one event that the test expects to be accepted.
func mustReduce(t *testing.T, p *Program, s State, e Event) State {
	t.Helper()
	n, _, err := Reduce(p, s, e, nil)
	if err != nil {
		t.Fatalf("%s: %v", e.Kind, err)
	}
	return n
}

// frame is what the window shows at one moment. blocked makes the status field
// unreadable behind an overlay (the target resolves to nothing and the text a
// recovery matches on is "blocked").
type frame struct {
	status  string
	balance int64
	blocked bool
}

// playResult is the whole run seen from outside the reducer.
type playResult struct {
	state    State
	counts   map[string]int
	captures map[string]captureInfo
	events   []Event
	commands []Command
	// violations lists every resolve that broke the reuse invariant, judged
	// from the command stream and this driver's own count of dispatches alone,
	// never from the reducer's state.
	violations []string
}

// captureInfo is what the desktop knows about a capture it handed out.
type captureInfo struct {
	kind      string // the command that took it
	era       int    // the oracle's era when it was taken
	sinceLast int    // 1 for the first capture after a dispatch, 2 for the next, ...
	frame     frame
}

// desk scripts a desktop. frameFor sees how many dispatches (acts of any kind,
// recoveries included) came before the capture and whether it is the first
// capture after one.
type desk struct {
	frameFor      func(era int, firstInEra bool, fresh int, target string) frame
	failWaitFirst int
	// mishap, when set, may spoil a command: "failed" (a capture fails),
	// "ambiguous", "declined" (a resolve is refused), "not_sent" or "external"
	// (an act did not go out). humanDispatch reports a person acting while the
	// run was paused.
	mishap        func(c Command) string
	humanDispatch func() bool
}

// play drives p to the end against d. Every command is answered by the scripted
// desktop; a resolve is answered from the frame of the capture it names. After
// each command the reuse invariant is checked on the stream itself: a resolve
// must name a capture taken after the last dispatch and not the first one taken
// after it.
func play(t *testing.T, p *Program, inputs map[string]Value, d desk) playResult {
	t.Helper()
	s, err := NewState(p, "run", "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	res := playResult{counts: map[string]int{}, captures: map[string]captureInfo{}}
	era, sinceLast, fresh, waitFails := 0, 0, 0, 0
	ev := Event{Kind: "start"}
	for guard := 0; guard < 400 && !s.Terminal(); guard++ {
		ev.Sequence, ev.RunID, ev.SessionID, ev.Epoch = s.Sequence+1, s.RunID, s.SessionID, s.Epoch
		next, cmds, err := Reduce(p, s, ev, inputs)
		if err != nil {
			if ev.Kind == "resume" {
				break // the attempt budget is spent: the run stays paused
			}
			t.Fatalf("event %d %s: %v", ev.Sequence, ev.Kind, err)
		}
		res.events = append(res.events, ev)
		s = next
		if len(cmds) == 0 {
			if s.Phase == Paused {
				if d.humanDispatch != nil {
					d.humanDispatch() // whether a person acted is not the oracle's business
				}
				// A resume restarts the oracle whether or not a person dispatched:
				// the run was paused, so anything may have changed on screen.
				era++
				sinceLast = 0
				ev = Event{Kind: "resume", NextEpoch: s.Epoch + 1}
			}
			continue
		}
		c := cmds[0]
		res.commands = append(res.commands, c)
		res.counts[c.Kind]++
		switch c.Kind {
		case "observe", "observe_after", "resolve":
			if c.Refocus {
				// The command activates the window first: the oracle's own rule, read
				// off the command stream, not off the reducer's state.
				era++
				sinceLast = 0
			}
			if c.Kind == "resolve" {
				info, ok := res.captures[c.ObservationID]
				switch {
				case !ok:
					res.violations = append(res.violations, fmt.Sprintf("resolve names unknown capture %q", c.ObservationID))
				case info.era != era:
					res.violations = append(res.violations, fmt.Sprintf("resolve names %q, taken before the last screen change", c.ObservationID))
				case info.sinceLast < 2:
					res.violations = append(res.violations, fmt.Sprintf("resolve names %q, the first capture after a screen change (%s)", c.ObservationID, info.kind))
				}
			}
			if c.Kind == "observe" && c.Step.ID == "settle" && waitFails < d.failWaitFirst {
				waitFails++
				ev = Event{Kind: "observe_failed", ActionID: c.ActionID, FailureCode: "capture_failed", Reason: "transient"}
				continue
			}
			ambiguous, incomplete := false, false
			if d.mishap != nil {
				switch d.mishap(c) {
				case "failed":
					if c.Kind != "resolve" {
						ev = Event{Kind: "observe_failed", ActionID: c.ActionID, FailureCode: "capture_failed", Reason: "transient"}
						continue
					}
					fallthrough
				case "declined":
					if c.Kind == "resolve" {
						ev = Event{Kind: "reuse_declined", ActionID: c.ActionID, Reason: "stale_observation"}
						continue
					}
				case "ambiguous":
					ambiguous = true
				case "incomplete":
					incomplete = true
				}
			}
			id := c.ObservationID
			if c.Kind != "resolve" {
				fresh++
				if !incomplete {
					sinceLast++ // an incomplete frame is not a usable frame
				}
				id = fmt.Sprintf("obs-%d", fresh)
				res.captures[id] = captureInfo{kind: c.Kind, era: era, sinceLast: sinceLast, frame: d.frameFor(era, sinceLast == 1, fresh, c.Step.Target)}
			}
			f := res.captures[id].frame
			o := Observation{ID: id, Complete: true, Matches: 1, TargetID: "node-" + c.Step.Target, Actionable: true, Target: c.Step.Target}
			switch c.Step.Kind {
			case "extract":
				o.Value = Value{Type: "money", Integer: f.balance, Currency: "USD"}
			case "assert", "branch", "wait":
				o.Value = Value{Type: "string", Text: f.status}
			}
			if f.blocked {
				// An overlay hides the target of whatever step this is.
				o.Matches, o.TargetID, o.Value = 0, "", Value{Type: "string", Text: "blocked"}
			}
			if ambiguous {
				o.Matches, o.TargetID = 2, ""
			}
			o.Complete = !incomplete
			ev = Event{Kind: "observed", ActionID: c.ActionID, Observation: o}
		case "act":
			era++ // any act restarts the oracle, whether or not it went out
			sinceLast = 0
			ev = Event{Kind: "receipt", ActionID: c.ActionID, Delivery: "sent"}
			if d.mishap != nil {
				switch d.mishap(c) {
				case "not_sent":
					ev = Event{Kind: "receipt", ActionID: c.ActionID, Delivery: "not_sent", FailureCode: "window_changed", Reason: "moved"}
				case "external":
					ev = Event{Kind: "receipt", ActionID: c.ActionID, Delivery: "not_sent", FailureCode: "external_interaction", Reason: "person"}
				}
			}
		case "request_approval":
			ev = Event{Kind: "approve", ActionID: c.ActionID, Observation: Observation{ID: s.Observation.ID}}
		default:
			t.Fatalf("unexpected command %q", c.Kind)
		}
	}
	res.state = s
	return res
}

// recoveryBank: press search, wait for the status, read the balance. If an
// overlay hides the status, a recovery presses dismiss.
func recoveryBank(t *testing.T) (*Program, map[string]Value) {
	t.Helper()
	c := Capability{SchemaVersion: 1, ID: "bank.recover", Revision: "1", Application: "bank",
		Targets: map[string]Selector{
			"search":  {Role: "button", Name: "Search"},
			"status":  {Role: "text_field", Name: "Status"},
			"balance": {Role: "text_field", Name: "Balance"},
			"dismiss": {Role: "button", Name: "Dismiss"},
		},
		Limits: DefaultLimits(),
		Steps: []Step{
			{ID: "search", Kind: "press", Target: "search", Effect: "read"},
			{ID: "settle", Kind: "wait", Target: "status", Effect: "read", Predicate: Predicate{Op: "contains", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "Member"}}}},
			{ID: "balance", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"},
		},
		Recoveries: []Recovery{{ID: "dismiss_overlay",
			When:   RecoveryWhen{Target: "status", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "blocked"}}}},
			Action: RecoveryAction{Kind: "press", Target: "dismiss"}, Max: 1}}}
	return mustCompile(t, c), nil
}
