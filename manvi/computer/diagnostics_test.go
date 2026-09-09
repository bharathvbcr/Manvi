package computer

import (
	"github.com/bharathvbcr/Manvi/manvi/workflow"
	"testing"
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
