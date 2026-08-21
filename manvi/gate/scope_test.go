package gate

import (
	"strings"
	"testing"
	"time"

	"manvi/dc"
	"manvi/grants"
	"manvi/policy"
)

func widenedTask() *dc.Task {
	task := testTask()
	task.AgentAppendedPlannedFiles = []dc.PlannedFile{
		{Path: "internal/helper.go", AllowedChange: dc.ChangeModify},
	}
	return task
}

// A run that leaned on scope the executor wrote for itself is not a run where
// the plan authorised every write, and the report has to be able to say so —
// including in a later process, where the grant that argued for it is gone.
func TestAWidenedWriteIsCountedAndKeepsTheRunOffStrict(t *testing.T) {
	g := newGate(t, nil)

	d, err := g.EvaluateWrite("internal/helper.go", widenedTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if d.Blocked() {
		t.Fatalf("appended scope authorises the write: %+v", d)
	}
	if d.Widened == "" || d.Clean() {
		t.Fatalf("the write must be marked and must not read as clean: %+v", d)
	}

	rep := g.Report()
	if rep.Widened != 1 {
		t.Fatalf("widened = %d, want the write counted", rep.Widened)
	}
	if rep.Clean != 0 {
		t.Fatalf("clean = %d, want a widened write counted as nothing else", rep.Clean)
	}
	if rep.Strict() {
		t.Fatal("a run carrying a self-widened write is not a strict run")
	}

	// The plan's own file is unaffected: it still passes cleanly.
	if planned, err := g.EvaluateWrite("src/calc.go", widenedTask(), dc.OpWrite); err != nil || !planned.Clean() {
		t.Fatalf("a planned write stays clean: %+v %v", planned, err)
	}
}

// The reason a widening exists lives in the grant that argued for it, and that
// grant is now typically never applied to any decision — the scope it caused to
// be written is what allows the write. A report assembled only from applied
// grants would show the widening with no record of why.
func TestAGrantThatOnlyWidenedScopeStillReportsItsReason(t *testing.T) {
	g := newGate(t, nil)

	blocked, err := g.EvaluateWrite("internal/helper.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !blocked.Overridable() {
		t.Fatalf("setup: expected an overridable block, got %+v", blocked)
	}
	if _, err := g.RequestOverride(blocked,
		grants.Grantor{Authority: grants.Agent, ID: "builder-1"},
		"the fix needs a helper the plan did not enumerate", testTask().ID); err != nil {
		t.Fatal(err)
	}

	// The next write is authorised by the appended scope, not by the grant.
	if d, err := g.EvaluateWrite("internal/helper.go", widenedTask(), dc.OpWrite); err != nil || d.GrantID != "" {
		t.Fatalf("the widening, not the grant, allows this: %+v %v", d, err)
	}

	rep := g.Report()
	if len(rep.GrantLines) != 0 {
		t.Fatalf("no grant was applied to a decision: %v", rep.GrantLines)
	}
	if len(rep.IssuedGrantLines) != 1 {
		t.Fatalf("issued grants = %v, want the one that was argued", rep.IssuedGrantLines)
	}
	line := rep.IssuedGrantLines[0]
	for _, want := range []string{"internal/helper.go", "scope.unplanned", "agent:builder-1", "did not enumerate"} {
		if !strings.Contains(line, want) {
			t.Errorf("the issued-grant line must carry %q: %q", want, line)
		}
	}
}

// A grant that did clear a decision stays where it always was, so the two
// summaries never double-count one grant.
func TestAnAppliedGrantIsNotAlsoReportedAsUnapplied(t *testing.T) {
	g := newGate(t, nil)
	blocked, _ := g.EvaluateWrite("internal/other.go", testTask(), dc.OpWrite)
	if _, err := g.RequestOverride(blocked,
		grants.Grantor{Authority: grants.Human, ID: "operator"}, "the plan omitted it", ""); err != nil {
		t.Fatal(err)
	}
	if d, _ := g.EvaluateWrite("internal/other.go", testTask(), dc.OpWrite); d.GrantID == "" {
		t.Fatalf("the grant should have cleared this: %+v", d)
	}

	rep := g.Report()
	if len(rep.GrantLines) != 1 {
		t.Fatalf("applied grants = %v, want one", rep.GrantLines)
	}
	if len(rep.IssuedGrantLines) != 0 {
		t.Fatalf("an applied grant must not also be listed as unapplied: %v", rep.IssuedGrantLines)
	}
}

// The rung that had no switch before. Turning it off restores the behaviour a
// repository without a code graph used to get.
func TestTheSameDirectoryRungCanBeSwitchedOff(t *testing.T) {
	on := newGate(t, nil)
	if d, err := on.EvaluateWrite("src/helper.go", testTask(), dc.OpWrite); err != nil || d.Blocked() {
		t.Fatalf("with the fallback on, a sibling of a planned file is allowed: %+v %v", d, err)
	}

	off := newGate(t, map[string]string{"policy.scope.allow_same_dir": "false"})
	d, err := off.EvaluateWrite("src/helper.go", testTask(), dc.OpWrite)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Blocked() || d.Rule != policy.RuleUnplannedScope {
		t.Fatalf("with the fallback off the write is refused: %+v", d)
	}
}

// A ledger reloaded from a previous run is not a record of what this run did.
// Without this the summary would replay every grant ever argued in the
// repository, every time, and the one line that mattered would be buried.
func TestARestoredLedgerIsNotReportedAsThisRunsWork(t *testing.T) {
	g := newGate(t, nil)
	g.Ledger.Restore([]grants.Grant{{
		ID:      "GRANT-0001",
		Grantor: grants.Grantor{Authority: grants.Human, ID: "operator"},
		Reason:  "argued last week",
		Scope: grants.Scope{
			TaskID: "TASK-OLD",
			Rules:  []policy.RuleID{policy.RuleUnplannedScope},
			Paths:  []string{"legacy/thing.go"},
		},
		IssuedAt:  time.Now().Add(-7 * 24 * time.Hour),
		ExpiresAt: time.Now().Add(time.Hour),
	}})

	if lines := g.Report().IssuedGrantLines; len(lines) != 0 {
		t.Fatalf("a run that issued nothing must report nothing: %v", lines)
	}

	blocked, _ := g.EvaluateWrite("internal/helper.go", testTask(), dc.OpWrite)
	if _, err := g.RequestOverride(blocked,
		grants.Grantor{Authority: grants.Agent, ID: "builder-1"}, "argued today", testTask().ID); err != nil {
		t.Fatal(err)
	}

	lines := g.Report().IssuedGrantLines
	if len(lines) != 1 || !strings.Contains(lines[0], "argued today") {
		t.Fatalf("issued grants = %v, want only what this run argued", lines)
	}

	// The restored grant is still a grant: it is in the ledger and it still
	// clears what it covers.
	if len(g.Ledger.All()) != 2 {
		t.Fatalf("the ledger holds %d grants, want the restored one kept", len(g.Ledger.All()))
	}
}
