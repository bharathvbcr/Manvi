package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixture(t *testing.T, change bool) *Program {
	t.Helper()
	effect := "read"
	if change {
		effect = "change"
	}
	c := Capability{SchemaVersion: 1, ID: "balance", Revision: "1", Application: "bank", Targets: map[string]Selector{"search": {Role: "button", Name: "Search"}, "balance": {Role: "text_field", Name: "Balance"}}, Limits: DefaultLimits(), Steps: []Step{{ID: "search", Kind: "press", Target: "search", Effect: effect}, {ID: "balance", Kind: "extract", Target: "balance", Effect: "read", Output: "balance", OutputType: "money", Currency: "USD"}}}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(b)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func inputEvent(s State, kind string) Event {
	return Event{Sequence: s.Sequence + 1, RunID: s.RunID, SessionID: s.SessionID, Epoch: s.Epoch, Kind: kind, ElapsedMillis: s.ElapsedMillis, ActionID: s.RunID + ":search"}
}
func start(t *testing.T, p *Program) State {
	t.Helper()
	s, err := NewState(p, "run", "session", 1)
	if err != nil {
		t.Fatal(err)
	}
	s, _, err = Reduce(p, s, inputEvent(s, "start"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func observed(s State) Event {
	e := inputEvent(s, "observed")
	e.Observation = Observation{ID: "obs1", TargetID: "node1", Matches: 1, Complete: true, Actionable: true}
	return e
}

func TestApprovalBindsEpochActionAndObservation(t *testing.T) {
	p := fixture(t, true)
	s := start(t, p)
	var err error
	s, _, err = Reduce(p, s, observed(s), nil)
	if err != nil || s.Phase != AwaitingApproval {
		t.Fatalf("%+v %v", s, err)
	}
	for _, mutate := range []func(*Event){func(e *Event) { e.Epoch++ }, func(e *Event) { e.ActionID = "other" }, func(e *Event) { e.Observation.ID = "stale" }, func(e *Event) { e.RunID = "other" }} {
		e := inputEvent(s, "approve")
		e.Observation = s.Observation
		mutate(&e)
		n, commands, err := Reduce(p, s, e, nil)
		if err == nil || n.Phase != s.Phase || len(commands) != 0 {
			t.Fatal("invalid approval dispatched")
		}
	}
	e := inputEvent(s, "approve")
	e.Observation = s.Observation
	n, commands, err := Reduce(p, s, e, nil)
	if err != nil || n.Phase != Acting || len(commands) != 1 {
		t.Fatal("valid approval rejected", err)
	}
	if _, _, err := Reduce(p, n, e, nil); err == nil {
		t.Fatal("approval replay accepted")
	}
}

func TestUnknownDeliveryNeverRetries(t *testing.T) {
	p := fixture(t, false)
	s := start(t, p)
	s, _, _ = Reduce(p, s, observed(s), nil)
	e := inputEvent(s, "receipt")
	e.Delivery = "unknown"
	n, commands, err := Reduce(p, s, e, nil)
	if err != nil || n.Phase != Unknown || len(commands) != 0 {
		t.Fatal("uncertain action retried")
	}
	if _, _, err := Reduce(p, n, inputEvent(n, "resume"), nil); err == nil {
		t.Fatal("unknown outcome silently resumed")
	}
}
func TestPostActionObservationBeforeAdvance(t *testing.T) {
	p := fixture(t, false)
	s := start(t, p)
	s, _, _ = Reduce(p, s, observed(s), nil)
	e := inputEvent(s, "receipt")
	e.Delivery = "sent"
	s, cmds, err := Reduce(p, s, e, nil)
	if err != nil || s.Phase != PostAction || cmds[0].Kind != "observe_after" {
		t.Fatal("receipt treated as success")
	}
	e = observed(s)
	e.Observation.TargetID = ""
	e.Observation.Matches = 0
	s, _, err = Reduce(p, s, e, nil)
	if err != nil || s.StepIndex != 1 {
		t.Fatal("fresh post-action observation rejected", err)
	}
	e = inputEvent(s, "observed")
	e.ActionID = "run:balance"
	e.Observation = Observation{ID: "obs3", TargetID: "balance", Complete: true, Matches: 1, Value: Value{Type: "money", Integer: 125000, Currency: "USD"}}
	s, cmds, err = Reduce(p, s, e, nil)
	if err != nil || s.Phase != Completed || s.Outputs["balance"].Integer != 125000 || len(cmds) != 0 {
		t.Fatal("extraction failed", err)
	}
}
func TestAmbiguousTargetPausesWithoutInput(t *testing.T) {
	p := fixture(t, false)
	s := start(t, p)
	for i := 0; i < 3; i++ {
		e := observed(s)
		e.Observation.Matches = 2
		var cmds []Command
		var err error
		s, cmds, err = Reduce(p, s, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, cmd := range cmds {
			if cmd.Kind == "act" {
				t.Fatal("ambiguous action dispatched")
			}
		}
	}
	if s.Phase != Paused {
		t.Fatal("did not bound observation retries")
	}
}
func TestCompileRejectsMalformedAndCyclicArtifacts(t *testing.T) {
	p := fixture(t, false)
	for _, raw := range [][]byte{[]byte(`{"schema_version":1,"schema_version":2}`), append(p.Bytes(), []byte(`{}`)...), []byte(strings.Replace(string(p.Bytes()), `"kind":"press"`, `"kind":"shell"`, 1)), []byte(strings.Replace(string(p.Bytes()), `"effect":"read"`, `"effect":"read","next":"search"`, 1))} {
		if _, err := Compile(raw); err == nil {
			t.Fatal("invalid capability accepted", string(raw))
		}
	}
}
func TestProgramAndStateDoNotAliasCaller(t *testing.T) {
	p := fixture(t, false)
	c := p.Capability()
	c.Targets["search"] = Selector{Name: "changed"}
	c.Steps[0].Kind = "shell"
	if p.Capability().Targets["search"].Name != "Search" || p.Capability().Steps[0].Kind != "press" {
		t.Fatal("program is mutable")
	}
	s := start(t, p)
	s.Outputs["existing"] = Value{Type: "string", Text: "original"}
	n, _, err := Reduce(p, s, observed(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	n.Outputs["existing"] = Value{Type: "string", Text: "mutated"}
	if s.Outputs["existing"].Text != "original" {
		t.Fatal("state aliases previous revision")
	}
}
func TestOfflineReplayReconstructsAndRejectsForeignEvents(t *testing.T) {
	p := fixture(t, false)
	initial, _ := NewState(p, "run", "session", 1)
	event := inputEvent(initial, "start")
	s, err := Replay(p, initial, []Event{event}, nil)
	if err != nil || s.Phase != Observing {
		t.Fatal(err)
	}
	event.SessionID = "foreign"
	if _, err := Replay(p, initial, []Event{event}, nil); err == nil {
		t.Fatal("foreign journal accepted")
	}
}
