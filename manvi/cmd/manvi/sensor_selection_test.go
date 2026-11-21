package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
)

// The selection rides on the report the verifier already returns and is
// recorded on the verify/report event; the sensor runs nothing for it.
func TestSensorRecordsTheTestSelectionAsSelectedNotRun(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictPassed, Source: "path-scoped gates",
		Selection: &devcouncil.TestSelection{Ran: true, Tests: []string{"pkg/a_test.go"}, Total: 3, Generation: 7, Stale: true},
	}}
	s, log := newSensor(t, v)

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"pkg/a.go"}}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	got := reports(t, log)
	if len(got) != 1 {
		t.Fatalf("reports = %+v", got)
	}
	d := got[0]
	for _, want := range []string{"Selected, not run", "generation 7, stale", "3 test(s)", "and 2 more", "may be incomplete"} {
		if !strings.Contains(d.TestSelection, want) {
			t.Errorf("test_selection %q lacks %q", d.TestSelection, want)
		}
	}
	if len(d.SelectedTests) != 1 || d.SelectedTotal != 3 {
		t.Fatalf("selected = %v of %d", d.SelectedTests, d.SelectedTotal)
	}
	if v.calls != 1 {
		t.Fatalf("the verifier ran %d times", v.calls)
	}
}

func TestSensorSaysWhenTheSelectionDidNotRun(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictPassed, Source: "path-scoped gates",
		Selection: &devcouncil.TestSelection{Notes: []string{"the code index is not configured"}},
	}}
	s, log := newSensor(t, v)

	if err := s.check(context.Background(), &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"a.go"}}); err != nil {
		t.Fatal(err)
	}

	d := reports(t, log)[0]
	if !strings.Contains(d.TestSelection, "did not run") || strings.Contains(d.TestSelection, "no tests") || len(d.SelectedTests) != 0 {
		t.Fatalf("test_selection = %q, selected %v", d.TestSelection, d.SelectedTests)
	}
}

func TestSensorBounceTextCarriesTheSelectionAndCaveatsForUnlistedChanges(t *testing.T) {
	v := &fakeVerifier{report: devcouncil.PathReport{
		Verdict: devcouncil.VerdictFailed, Source: "operator verification command",
		Findings:  []string{"the verification command exited 1"},
		Selection: &devcouncil.TestSelection{Ran: true, Tests: []string{"pkg/a_test.go"}, Total: 1, Generation: 4},
	}}
	s, log := newSensor(t, v)
	s.command = "make check"

	e := &agent.TurnStopping{Turn: 1, Mutated: true, Wrote: []string{"pkg/a.go"}, UnenumeratedEffects: true}
	if err := s.check(context.Background(), e); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"Selected, not run", "pkg/a_test.go", "could not be listed"} {
		if !strings.Contains(e.Inject, want) {
			t.Errorf("bounce text lacks %q:\n%s", want, e.Inject)
		}
	}
	if v.gotCmd != "make check" {
		t.Fatalf("the operator command was not handed through: %q", v.gotCmd)
	}
	if !strings.Contains(reports(t, log)[0].TestSelection, "could not be listed") {
		t.Fatal("the recorded selection omits the unlisted-changes caveat")
	}
}
