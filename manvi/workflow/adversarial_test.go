package workflow

import (
	"encoding/json"
	"testing"
)

func compileCapability(c Capability) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = Compile(raw)
	return err
}

func TestCompilerRejectsUnavailableOrAmbiguousOutputReferences(t *testing.T) {
	base := fixture(t, true).Capability()
	for _, which := range []string{"missing", "forward", "branch", "duplicate"} {
		t.Run(which, func(t *testing.T) {
			c := base
			read := Step{ID: "read", Kind: "extract", Target: "balance", Effect: "read", Output: "value", OutputType: "string"}
			check := Step{ID: "check", Kind: "assert", Target: "balance", Effect: "read", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "output", Key: "value"}}}
			switch which {
			case "missing":
				c.Steps = []Step{check}
			case "forward":
				c.Steps = []Step{check, read}
			case "branch":
				c.Steps = []Step{{ID: "branch", Kind: "branch", Target: "balance", Effect: "read", Predicate: Predicate{Op: "exists"}, Otherwise: "check"}, read, check}
			case "duplicate":
				second := read
				second.ID = "second"
				c.Steps = []Step{read, second, check}
			}
			if err := compileCapability(c); err == nil {
				t.Fatal("invalid output flow compiled")
			}
		})
	}
}

func TestCompilerRequiresTypedScrollAndAllowsReviewedReadEntry(t *testing.T) {
	base := fixture(t, true).Capability()
	for _, kind := range []string{"set_value", "type_text", "scroll"} {
		t.Run(kind, func(t *testing.T) {
			c := base
			c.Steps = append([]Step(nil), base.Steps...)
			c.Steps[0].Kind = kind
			c.Steps[0].Effect = "read"
			c.Steps[0].Input = Ref{Source: "literal", Literal: Value{Type: "string", Text: "value"}}
			if err := compileCapability(c); kind == "scroll" && err == nil {
				t.Fatal("noninteger scroll compiled")
			} else if kind != "scroll" && err != nil {
				t.Fatalf("reviewed read-only form entry rejected: %v", err)
			}
		})
	}
}

func TestResumeAfterSentActionOnlyReobserves(t *testing.T) {
	p := fixture(t, false)
	s := start(t, p)
	var err error
	s, _, err = Reduce(p, s, observed(s), nil)
	if err != nil {
		t.Fatal(err)
	}
	e := inputEvent(s, "receipt")
	e.Delivery = "sent"
	s, _, err = Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	e = inputEvent(s, "pause")
	e.NextEpoch = 2
	s, _, err = Reduce(p, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	e = inputEvent(s, "resume")
	e.NextEpoch = 3
	s, cmds, err := Reduce(p, s, e, nil)
	if err != nil || s.Phase != PostAction || len(cmds) != 1 || cmds[0].Kind != "observe_after" {
		t.Fatalf("sent action would be executed again after resume: %+v %+v %v", s, cmds, err)
	}
}

func TestWaitPollsFreshObservationsAndPausesAtBound(t *testing.T) {
	c := fixture(t, false).Capability()
	c.Steps = []Step{{ID: "ready", Kind: "wait", Target: "balance", Effect: "read", Predicate: Predicate{Op: "equals", Expected: Ref{Source: "literal", Literal: Value{Type: "string", Text: "Ready"}}}}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	p, err := Compile(raw)
	if err != nil {
		t.Fatal(err)
	}
	s := start(t, p)
	for i := 0; i < c.Limits.ObservationAttempts; i++ {
		e := observed(s)
		e.ActionID = s.ActionID(p)
		e.Observation.Value = Value{Type: "string", Text: "Loading"}
		next, cmds, err := Reduce(p, s, e, nil)
		if err != nil {
			t.Fatal(err)
		}
		s = next
		if i < c.Limits.ObservationAttempts-1 {
			if s.Phase != Observing || len(cmds) != 1 || cmds[0].Kind != "observe" {
				t.Fatal("wait failed before retry bound")
			}
		} else if s.Phase != Paused || len(cmds) != 0 {
			t.Fatal("wait did not pause at retry bound")
		}
	}
	s = start(t, p)
	e := observed(s)
	e.ActionID = s.ActionID(p)
	e.Observation.Value = Value{Type: "string", Text: "Ready"}
	s, _, err = Reduce(p, s, e, nil)
	if err != nil || s.Phase != Completed {
		t.Fatal("satisfied wait did not complete", err)
	}
}
