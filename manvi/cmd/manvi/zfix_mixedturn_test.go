package main

import (
	"context"
	"testing"

	"manvi/agent"
	"manvi/devcouncil"
)

// A turn that edits one file natively and changes another through the shell is
// the case a non-empty Wrote list hides.
//
// The old fallback asked whether the list was empty. Here it is not — a.go is
// in it — so the degradation never fired, the gates read a.go, found it clean,
// and the turn was certified. Nothing had looked at what the command did. A
// non-empty list is evidence about the paths in it and about nothing else.
func TestMixedNativeAndShellTurnIsNotCertified(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict:  devcouncil.VerdictPassed,
		Examined: []string{"a.go"},
	}}
	s, log := newSensor(t, v)

	e := &agent.TurnStopping{
		Turn:                1,
		Mutated:             true,
		Wrote:               []string{"a.go"},
		UnenumeratedEffects: true,
	}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorDegraded {
		t.Fatalf("verdict = %q, want degraded: a command changed something no handler named, "+
			"and examining a.go says nothing about it", e.Verdict)
	}
	got := reports(t, log)
	if len(got) != 1 || len(got[0].Degraded) == 0 {
		t.Fatalf("reports = %+v, want the uncovered command effects recorded", got)
	}
}

// The counterpart, so the check above is a check and not a blanket degradation:
// a turn whose every change was named by a handler still passes.
func TestFullyEnumeratedTurnStillPasses(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict:  devcouncil.VerdictPassed,
		Examined: []string{"a.go"},
	}}
	s, _ := newSensor(t, v)

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if e.Verdict != agent.SensorPassed {
		t.Fatalf("verdict = %q, want passed: every change this turn made was named and examined",
			e.Verdict)
	}
}

// The baseline has to be recorded at the start of the turn and reach the check
// at the end of it. Captured late it describes the finish line; not carried
// across, it may as well not have been taken.
func TestBaselineIsCapturedAtTurnStartAndUsedAtTheCheck(t *testing.T) {
	v := &fakeVerifier{
		report:   devcouncil.PathReport{Verdict: devcouncil.VerdictPassed},
		baseline: devcouncil.Baseline{Tree: "deadbeef"},
	}
	s, _ := newSensor(t, v)

	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}
	if v.captures != 1 {
		t.Fatalf("the baseline was captured %d times at turn start, want 1", v.captures)
	}

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}
	if v.gotBaseline.Tree != "deadbeef" {
		t.Fatalf("the check ran against baseline %+v, not the one recorded at turn start",
			v.gotBaseline)
	}
}

// A second turn must not inherit the first turn's baseline. Doing so would
// attribute this turn's changes against a tree two turns old — the same
// attribution defect the baseline exists to fix, with a longer reach.
func TestBaselineIsNotReusedAcrossTurns(t *testing.T) {
	v := &fakeVerifier{
		report:   devcouncil.PathReport{Verdict: devcouncil.VerdictPassed},
		baseline: devcouncil.Baseline{Tree: "deadbeef"},
	}
	s, _ := newSensor(t, v)

	if err := s.begin(context.Background(), &agent.TurnStarting{Turn: 1}); err != nil {
		t.Fatal(err)
	}
	first := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}
	if err := s.check(context.Background(), first); err != nil {
		t.Fatal(err)
	}

	// A second turn whose start event never reached this listener.
	second := &agent.TurnStopping{Turn: 2, Mutated: true, Wrote: []string{"b.go"}}
	if err := s.check(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	if v.gotBaseline.Captured() {
		t.Fatalf("turn 2 reused turn 1's baseline: %+v", v.gotBaseline)
	}
	if v.gotBaseline.Note == "" {
		t.Fatal("a turn running without a baseline must say so, not pass an empty struct along")
	}
}
