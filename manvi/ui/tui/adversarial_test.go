package tui

import (
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// AgentView.Apply folds Degraded, GrantID and Weakened into Status before and
// regardless of Scrollback.Append's WireOnly skip. verify.report now carries
// Degraded (the checks that could not run), and the live path is
// ProjectSink -> Runner.Sink -> ActionEvent -> AgentView.Apply, so a verify
// report with a degraded entry flips StrictSummary and bumps the "N unrun" chip
// in addition to the outcomeNotices report that already did the same thing.
func TestAdversarialWireOnlyVerifyReportDoesNotMoveStatusChips(t *testing.T) {
	v := NewAgentView("t", "s")
	v.Status.Posture = "strict"
	v.Apply(ui.Event{
		Kind: ui.KindVerifyReport, Verdict: "passed",
		Degraded: []string{"secret_scan could not run"},
	})
	if len(v.Status.Degraded) != 0 {
		t.Errorf("a wire-only verify.report put %v in Status.Degraded; the terminal must not draw it", v.Status.Degraded)
	}
	if strict, why := v.Status.StrictSummary(); !strict {
		t.Errorf("a wire-only verify.report made the session non-strict: %s", why)
	}
}

// A child's forwarded verify.report goes to the parent sink with Agent set; the
// same applies.
func TestAdversarialForwardedChildVerifyReportDoesNotMarkParentDegraded(t *testing.T) {
	v := NewAgentView("t", "s")
	v.Apply(ui.Event{
		Kind: ui.KindVerifyReport, Agent: "critic", Verdict: "degraded",
		Degraded: []string{"end-of-turn check could not run"},
	})
	if len(v.Status.Degraded) != 0 {
		t.Errorf("parent status degraded by a child's wire-only report: %v", v.Status.Degraded)
	}
}
