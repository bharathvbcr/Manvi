package main

import (
	"strings"
	"testing"
)

// TestSubAgentsInheritTheEffortCeiling closes the second caller of the same
// escalation seam.
//
// A stuck turn may climb the effort ladder up to llm.effort.max. Sub-agents run
// the same loop against the same model and get stuck the same way, but the
// child's agent.Config was built with Effort and no EffortCeiling — so a
// fan-out silently opted out of the mechanism its parent was configured for.
// That is the shape of bug the notice consolidation already removed once: one
// behaviour, wired at one call site out of two.
func TestSubAgentsInheritTheEffortCeiling(t *testing.T) {
	src, err := readSource("cmd/manvi/subagent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "EffortCeiling:") {
		t.Fatal("the child loop is built without EffortCeiling: a sub-agent that " +
			"starts going in circles can never raise its effort, however the " +
			"parent was configured")
	}
	if !strings.Contains(src, "effortCeiling") {
		t.Fatal("subAgentConfig carries no effort ceiling, so the callers have " +
			"nothing to pass")
	}

	// Both real call sites must actually supply it, or the field is a
	// decoration that defaults to empty and disables escalation anyway.
	for _, f := range []string{"cmd/manvi/run.go", "cmd/manvi/tui.go"} {
		caller, err := readSource(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(caller, "effortCeiling:") {
			t.Errorf("%s attaches the sub-agent runner without an effort ceiling", f)
		}
	}
}

// TestSubAgentsReportThroughTheSharedNotices closes the third caller of the
// seam outcomeNotices was extracted to own.
//
// That function's doc says it exists because "there were two hand-rolled copies
// of that answer" in the two faces. There were three. subagent.go had its own,
// covering an empty summary, a truncated answer and the step ceiling — three of
// the dozen signals the Outcome carries. So a child whose every write the gate
// refused, whose server could not parse tool calls, whose prefill declaration
// was contradicted, or whose tool pipeline panicked returned fluent prose with
// no trace of any of it, and the dispatching model consumed that prose as fact.
//
// Structural rather than behavioural, in the same style and for the same reason
// as the effort-ceiling test above: the defect is a call site that does not
// reach the shared owner, and that is visible in the source.
func TestSubAgentsReportThroughTheSharedNotices(t *testing.T) {
	src, err := readSource("cmd/manvi/subagent.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "outcomeNotices(outcome") {
		t.Fatal("the sub-agent summary is built without outcomeNotices, so it reports " +
			"whichever signals this file happens to remember — which is the drift " +
			"outcomeNotices was extracted to end")
	}
	// The hand-rolled branches must be gone, not merely supplemented: two
	// sources for one answer is the condition, and duplicating a notice is its
	// own bug.
	for _, gone := range []string{
		"stopped at the output cap; it is cut off",
		"stopped at its %d-step ceiling",
	} {
		if strings.Contains(src, gone) {
			t.Errorf("subagent.go still hand-rolls %q beside outcomeNotices, so that signal "+
				"is reported twice and the next one added is reported once", gone)
		}
	}
}
