package computer

import (
	"errors"
	"strings"
	"testing"

	"github.com/bharathvbcr/Manvi/manvi/workflow"
)

func TestConfidenceDoesNotHideAmbiguityOrIncompleteEvidence(t *testing.T) {
	o := Observation{Complete: true, Nodes: []Node{{ID: "1", Role: "button", Name: "Confirm creation", Enabled: true}, {ID: "2", Role: "button", Name: "Confirm creation", Enabled: true}, {ID: "3", Role: "button", Name: "Back", Enabled: true}}}
	s := workflow.Selector{Role: "button", Name: "Confirm creation"}
	d := Diagnose(s, o)
	if d.ExactMatches != 2 || d.Confidence != "ambiguous" || d.Candidates[0].Score != 1 {
		t.Fatalf("score hid ambiguity: %+v", d)
	}
	o.Complete = false
	if Diagnose(s, o).Confidence != "incomplete_observation" {
		t.Fatal("incomplete capture presented as confident")
	}
}
func TestDiagnosticAncestryIsBoundedAndRequired(t *testing.T) {
	parent := "1"
	o := Observation{Complete: true, Nodes: []Node{{ID: "1", Name: "Cycle", Parent: &parent}, {ID: "2", Role: "button", Name: "Search", Parent: &parent}}}
	d := Diagnose(workflow.Selector{Role: "button", Name: "Search", Ancestor: "Bank"}, o)
	if d.ExactMatches != 0 || d.Candidates[0].Score >= 1 {
		t.Fatal("missing ancestor admitted")
	}
}
func TestExplainResolveUsesDiagnoseNotTheBrokerMessageAlone(t *testing.T) {
	o := Observation{Complete: true, Nodes: []Node{{ID: "1", Role: "button", Name: "Search", Enabled: true}}}
	reason, matches, handled := ExplainResolve(&BrokerError{Code: "target_missing", Delivery: "not_sent", Message: "gone"}, workflow.Selector{Name: "Ghost"}, o)
	if !handled || matches != 0 || !strings.Contains(reason, "match=missing") {
		t.Fatalf("missing target was not explained: %q matches=%d handled=%v", reason, matches, handled)
	}
	reason, matches, handled = ExplainResolve(&BrokerError{Code: "target_ambiguous", Delivery: "not_sent", Message: "many"}, workflow.Selector{Role: "button", Name: "Confirm creation"}, Observation{
		Complete: true,
		Nodes: []Node{
			{ID: "1", Role: "button", Name: "Confirm creation", Enabled: true},
			{ID: "2", Role: "button", Name: "Confirm creation", Enabled: true},
		},
	})
	if !handled || matches != 2 || !strings.Contains(reason, "match=ambiguous") {
		t.Fatalf("ambiguous target was not explained: %q matches=%d handled=%v", reason, matches, handled)
	}
	if _, _, handled = ExplainResolve(errors.New("timeout"), workflow.Selector{}, o); handled {
		t.Fatal("a non-matcher failure was treated as a waitable miss")
	}
}

func TestVisualAndEmptySelectorsCannotBorrowSemanticConfidence(t *testing.T) {
	o := Observation{Complete: true, Nodes: []Node{{ID: "one", Role: "button", Name: "Search", Enabled: true}, {ID: "two", Role: "text_field", Name: "Balance"}}}
	for _, s := range []workflow.Selector{{Visual: &workflow.VisualAnchor{}}, {}} {
		d := Diagnose(s, o)
		if d.ExactMatches != 0 || d.TotalCandidates != 0 || len(d.Candidates) != 0 {
			t.Fatalf("selector borrowed semantic confidence: %+v", d)
		}
		if s.Visual != nil && d.Confidence != "visual_requires_native_resolution" {
			t.Fatalf("visual authority claim: %+v", d)
		}
	}
}

func TestDryResolveLadderReportsRungIndexAndAmbiguity(t *testing.T) {
	o := Observation{Complete: true, Nodes: []Node{
		{ID: "1", Role: "button", Name: "Find member", Enabled: true},
		{ID: "2", Role: "button", Name: "Find member", Enabled: true},
		{ID: "3", Role: "button", Name: "Search", Enabled: true},
	}}
	hit := DryResolveLadder("search", workflow.Selector{
		Strategies: []workflow.Selector{
			{Role: "button", Name: "Ghost"},
			{Role: "button", Name: "Search"},
		},
		Rationale: "ghost then search",
		Stability: "semantic",
	}, o)
	if !hit.Found || hit.StrategyIndex != 1 || hit.Matches != 1 || hit.Ambiguous {
		t.Fatalf("expected rung 1 unique hit: %+v", hit)
	}
	hit = DryResolveLadder("search", workflow.Selector{
		Strategies: []workflow.Selector{
			{Role: "button", Name: "Find member"},
			{Role: "button", Name: "Search"},
		},
		Rationale: "ambiguous first",
		Stability: "semantic",
	}, o)
	if hit.Found || !hit.Ambiguous || hit.StrategyIndex != 0 || hit.Matches != 2 {
		t.Fatalf("expected ambiguous stop at rung 0: %+v", hit)
	}
	missing := DryResolveLadder("ghost", workflow.Selector{Role: "button", Name: "Missing"}, o)
	if missing.Found || missing.StrategyIndex != -1 || missing.Reason != "missing" {
		t.Fatalf("expected missing: %+v", missing)
	}
}
