package workflow

import (
	"encoding/json"
	"fmt"
	"testing"
)

// bankBalance is the bank.balance capability shape the product records: two
// input-bearing presses, a wait, a branch and an extract, all read-effect and
// all semantic.
const bankBalance = `{
  "schema_version":1,"id":"bank.balance","revision":"1","application":"jarvis-bank",
  "parameters":[{"name":"member_id","type":"string","sensitive":true}],
  "targets":{
    "member":{"strategies":[{"role":"text_field","name":"Member ID"}],"rationale":"member field","stability":"semantic"},
    "search":{"strategies":[{"role":"button","name":"Search"}],"rationale":"search","stability":"semantic"},
    "status":{"strategies":[{"role":"text_field","name":"Status"}],"rationale":"status","stability":"semantic"},
    "balance":{"strategies":[{"role":"text_field","name":"Balance"}],"rationale":"balance","stability":"semantic"}
  },
  "outputs":[{"name":"balance","type":"money","currency":"USD","description":"USD balance"}],
  "outcomes":[
    {"id":"success","kind":"success","description":"found","outputs":["balance"]},
    {"id":"not_found","kind":"business","description":"missing","outputs":[]}
  ],
  "steps":[
    {"id":"member","kind":"set_value","target":"member","effect":"read","input":{"source":"input","key":"member_id"}},
    {"id":"search","kind":"press","target":"search","effect":"read"},
    {"id":"settle","kind":"wait","target":"status","effect":"read","predicate":{"op":"contains","expected":{"source":"literal","literal":{"type":"string","text":"Member"}}}},
    {"id":"route","kind":"branch","target":"status","effect":"read","predicate":{"op":"equals","expected":{"source":"literal","literal":{"type":"string","text":"Member found"}}},"next":"balance","otherwise":"not_found"},
    {"id":"balance","kind":"extract","target":"balance","effect":"read","output":"balance","output_type":"money","currency":"USD","next":"ok"},
    {"id":"ok","kind":"conclude","outcome":"success"},
    {"id":"not_found","kind":"conclude","outcome":"not_found"}
  ],
  "limits":{"max_actions":40,"active_seconds":300,"observation_attempts":3,"intervention_seconds":600}
}`

func compileBank(t *testing.T) (*Program, map[string]Value) {
	t.Helper()
	p, err := Compile([]byte(bankBalance))
	if err != nil {
		t.Fatal(err)
	}
	return p, map[string]Value{"member_id": {Type: "string", Text: "M-1001"}}
}

// drive plays a desktop that always answers cleanly. It uses only command and
// event kind strings, so it runs unchanged against the reducer as it was before
// observations were reused. Fresh captures (observe, observe_after) take a new
// observation id; a resolve answers with the id it was given.
func drive(t *testing.T, p *Program, inputs map[string]Value) (State, map[string]int, []Event) {
	t.Helper()
	s, err := NewState(p, "run", "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var events []Event
	fresh := 0
	ev := Event{Kind: "start"}
	for guard := 0; guard < 200 && !s.Terminal(); guard++ {
		ev.Sequence, ev.RunID, ev.SessionID, ev.Epoch = s.Sequence+1, s.RunID, s.SessionID, s.Epoch
		next, cmds, err := Reduce(p, s, ev, inputs)
		if err != nil {
			t.Fatalf("event %d %s: %v", ev.Sequence, ev.Kind, err)
		}
		events = append(events, ev)
		s = next
		if len(cmds) == 0 {
			continue
		}
		c := cmds[0]
		counts[c.Kind]++
		switch c.Kind {
		case "observe", "observe_after", "resolve":
			id := c.ObservationID
			if c.Kind != "resolve" {
				fresh++
				id = fmt.Sprintf("obs-%d", fresh)
			}
			o := Observation{ID: id, Complete: true, Matches: 1, TargetID: "node-" + c.Step.Target, Actionable: true, Target: c.Step.Target}
			switch c.Step.Kind {
			case "extract":
				o.Value = Value{Type: "money", Integer: 125000, Currency: "USD"}
			case "assert", "branch", "wait":
				o.Value = Value{Type: "string", Text: "Member found"}
			}
			ev = Event{Kind: "observed", ActionID: c.ActionID, Observation: o}
		case "act":
			ev = Event{Kind: "receipt", ActionID: c.ActionID, Delivery: "sent"}
		case "request_approval":
			ev = Event{Kind: "approve", ActionID: c.ActionID, Observation: Observation{ID: s.Observation.ID}}
		default:
			t.Fatalf("unexpected command %q", c.Kind)
		}
	}
	return s, counts, events
}

func TestReplayAcceptsAnEventThatReusesAnObservationID(t *testing.T) {
	p, inputs := compileBank(t)
	final, _, events := drive(t, p, inputs)
	ids := map[string]int{}
	for _, e := range events {
		if e.Kind == "observed" {
			ids[e.Observation.ID]++
		}
	}
	reused := false
	for _, n := range ids {
		reused = reused || n > 1
	}
	if !reused {
		t.Fatal("fixture run reused no observation id")
	}
	initial, _ := NewState(p, "run", "session", 1)
	got, err := Replay(p, initial, events, inputs)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(final)
	if string(a) != string(b) {
		t.Fatalf("replay disagrees:\n%s\n%s", a, b)
	}
}

// atBoundary drives the two-step fixture past its first dispatch and returns the
// state and commands after the post-action capture is accepted.
func atBoundary(t *testing.T, p *Program) (State, []Command) {
	t.Helper()
	s := start(t, p)
	s, _, _ = Reduce(p, s, observed(s), nil)
	if s.Phase == AwaitingApproval {
		e := inputEvent(s, "approve")
		e.Observation = s.Observation
		s, _, _ = Reduce(p, s, e, nil)
	}
	r := inputEvent(s, "receipt")
	r.Delivery = "sent"
	s, _, _ = Reduce(p, s, r, nil)
	if s.Phase != PostAction {
		t.Fatalf("phase %s", s.Phase)
	}
	after := inputEvent(s, "observed")
	after.Observation = Observation{ID: "after1", Complete: true}
	s, cmds, err := Reduce(p, s, after, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, cmds
}

// A step right after a dispatch always observes afresh, even a read step.
func TestFirstStepAfterADispatchObservesFresh(t *testing.T) {
	p := fixture(t, false)
	s, cmds := atBoundary(t, p)
	if len(cmds) != 1 || cmds[0].Kind != "observe" || cmds[0].ObservationID != "" || s.Reuse != "" {
		t.Fatalf("the post-action capture must not be reused: %+v %+v", s, cmds)
	}
}

// twoReads: press, then two read steps. The second reads the first one's
// snapshot.
func twoReads(t *testing.T) *Program {
	t.Helper()
	c := Capability{SchemaVersion: 1, ID: "tworeads", Revision: "1", Application: "bank",
		Targets: map[string]Selector{"search": {Role: "button", Name: "Search"}, "balance": {Role: "text_field", Name: "Balance"}},
		Limits:  DefaultLimits(),
		Steps: []Step{{ID: "search", Kind: "press", Target: "search", Effect: "read"},
			{ID: "first", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"},
			{ID: "second", Kind: "extract", Target: "balance", Effect: "read", Output: "again", OutputType: "money", Currency: "USD"}}}
	raw, _ := json.Marshal(c)
	p, err := Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func answerWith(t *testing.T, p *Program, s State, e Event) (State, []Command) {
	t.Helper()
	e.Sequence, e.RunID, e.SessionID, e.Epoch = s.Sequence+1, s.RunID, s.SessionID, s.Epoch
	if e.ActionID == "" {
		e.ActionID = s.ActionID(p)
	}
	n, cmds, err := Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	return n, cmds
}

// atSnapshot returns a run with a resolve pending against the fresh capture
// "snap1" that the first read step was just decided on.
func atSnapshot(t *testing.T) (*Program, State) {
	t.Helper()
	p := twoReads(t)
	s, cmds := atBoundary(t, p)
	money := Value{Type: "money", Integer: 5, Currency: "USD"}
	s, cmds = answerWith(t, p, s, Event{Kind: "observed", Observation: Observation{ID: "snap1", Complete: true, Matches: 1, TargetID: "n", Actionable: true, Value: money}})
	if len(cmds) != 1 || cmds[0].Kind != "resolve" || cmds[0].ObservationID != "snap1" || s.Reuse != "snap1" {
		t.Fatalf("the second read step must resolve against the first one's fresh capture: %+v %+v", s, cmds)
	}
	return p, s
}

func requireFresh(t *testing.T, what string, s State, cmds []Command) {
	t.Helper()
	if len(cmds) != 1 || cmds[0].Kind != "observe" || cmds[0].ObservationID != "" || cmds[0].DelayMillis != 0 || s.Attempts != 0 || s.Reuse != "" || s.Phase != Observing {
		t.Fatalf("%s: want one plain fresh observe, no delay, no attempt used; got %+v %+v", what, s, cmds)
	}
}

func TestReuseFallsBackToAFreshObserve(t *testing.T) {
	good := Observation{ID: "snap1", Complete: true, Matches: 1, TargetID: "n", Actionable: true, Value: Value{Type: "money", Integer: 5, Currency: "USD"}}
	cases := map[string]Event{
		"stale or foreign capture": {Kind: "reuse_declined", Reason: "stale_observation"},
		"ambiguous resolve":        {Kind: "observed", Observation: func() Observation { o := good; o.Matches = 2; return o }()},
		"missing target":           {Kind: "observed", Observation: func() Observation { o := good; o.Matches = 0; o.TargetID = ""; return o }()},
		"incomplete":               {Kind: "observed", Observation: func() Observation { o := good; o.Complete = false; return o }()},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			p, s := atSnapshot(t)
			n, cmds := answerWith(t, p, s, e)
			requireFresh(t, name, n, cmds)
		})
	}
	t.Run("clean answer is accepted", func(t *testing.T) {
		p, s := atSnapshot(t)
		n, cmds := answerWith(t, p, s, Event{Kind: "observed", Observation: good})
		if n.Phase != Completed || len(cmds) != 0 || n.Reuse != "" || n.Outputs["again"].Integer != 5 {
			t.Fatalf("%+v %+v", n, cmds)
		}
	})
	t.Run("decline without a pending resolve is refused", func(t *testing.T) {
		p := fixture(t, false)
		s := start(t, p)
		e := inputEvent(s, "reuse_declined")
		e.ActionID = s.ActionID(p)
		if _, _, err := Reduce(p, s, e, nil); err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("pause clears the pending reuse", func(t *testing.T) {
		p, s := atSnapshot(t)
		n, _ := answerWith(t, p, s, Event{Kind: "pause", NextEpoch: s.Epoch + 1})
		if n.Reuse != "" || n.Phase != Paused {
			t.Fatalf("%+v", n)
		}
	})
}

// A reused frame is only accepted when it confirms a checkpoint: an unsatisfied
// predicate is decided again on a fresh observation.
func TestReusedFrameNeverFailsOrReroutesACheckpoint(t *testing.T) {
	p, inputs := compileBank(t)
	for _, step := range []string{"settle", "route"} {
		s, _ := NewState(p, "run", "session", 1)
		s.StepIndex = p.positions[step]
		s.Phase = Observing
		s.Reuse = "after1"
		e := Event{Sequence: 1, RunID: "run", SessionID: "session", Epoch: 1, Kind: "observed", ActionID: s.ActionID(p),
			Observation: Observation{ID: "after1", Complete: true, Matches: 1, TargetID: "n", Actionable: true, Value: Value{Type: "string", Text: "Searching"}}}
		n, cmds, err := Reduce(p, s, e, inputs)
		if err != nil {
			t.Fatal(err)
		}
		requireFresh(t, step, n, cmds)
	}
}

func TestApprovalStepsAreNeverResolvedAgainstAReusedFrame(t *testing.T) {
	c := Capability{SchemaVersion: 1, ID: "two", Revision: "1", Application: "bank",
		Targets: map[string]Selector{"a": {Role: "button", Name: "A"}, "b": {Role: "button", Name: "B"}, "balance": {Role: "text_field", Name: "Balance"}},
		Limits:  DefaultLimits(),
		Steps: []Step{{ID: "search", Kind: "press", Target: "a", Effect: "read"}, {ID: "b", Kind: "press", Target: "b", Effect: "change"},
			{ID: "balance", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"}}}
	raw, _ := json.Marshal(c)
	p, err := Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	s, cmds := atBoundary(t, p)
	if len(cmds) != 1 || cmds[0].Kind != "observe" || s.Reuse != "" {
		t.Fatalf("an effect:change step must be shown a fresh observation: %+v %+v", s, cmds)
	}
}

func TestApprovalRevalidationStillReobserves(t *testing.T) {
	p := fixture(t, true)
	s := start(t, p)
	s, _, _ = Reduce(p, s, observed(s), nil)
	e := inputEvent(s, "approve")
	e.Observation = s.Observation
	s, _, _ = Reduce(p, s, e, nil)
	changed := inputEvent(s, "approval_changed")
	n, cmds, err := Reduce(p, s, changed, nil)
	if err != nil || len(cmds) != 1 || cmds[0].Kind != "observe" || n.Phase != Observing || n.Observation.ID != "" {
		t.Fatalf("%+v %+v %v", n, cmds, err)
	}
}
