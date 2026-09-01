package dc

import (
	"encoding/json"
	"strings"
	"testing"
)

// The default that would have silently inverted.
//
// DevCouncil declares `required: bool = True`, and pydantic omits defaults when
// it serialises. Go zeroes an absent bool to false, so decoding with the plain
// struct tag turns every criterion a producer left the key off into an optional
// one. Nothing downstream would report it: the criterion is still listed, still
// looks checked, and has simply stopped being something the work must satisfy.
func TestAnAbsentRequiredFlagMeansRequired(t *testing.T) {
	var ac AcceptanceCriterion
	if err := json.Unmarshal([]byte(
		`{"id":"AC-1","description":"adds","verification_method":"unit_test"}`), &ac); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if !ac.Required {
		t.Error("a criterion with no `required` key decoded as optional; DevCouncil's default is true")
	}
}

// An explicit false is still false -- the default must not overwrite a stated
// intention.
func TestAnExplicitFalseRequiredSurvives(t *testing.T) {
	var ac AcceptanceCriterion
	if err := json.Unmarshal([]byte(
		`{"id":"AC-1","description":"nice to have","verification_method":"manual","required":false}`), &ac); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if ac.Required {
		t.Error("an explicit required:false was overwritten by the default")
	}
}

// A verification method no gate implements must not decode.
//
// The alternative is a criterion that sits in the plan looking checkable while
// no gate will ever claim it -- an acceptance criterion that can never fail,
// which is worse than one that is missing.
func TestAnUnknownVerificationMethodIsRefused(t *testing.T) {
	var ac AcceptanceCriterion
	err := json.Unmarshal([]byte(
		`{"id":"AC-1","description":"x","verification_method":"vibes"}`), &ac)
	if err == nil {
		t.Fatal("an unknown verification_method decoded successfully")
	}
	if !strings.Contains(err.Error(), "AC-1") {
		t.Errorf("the error must name the criterion so it is actionable: %v", err)
	}
}

// DevCouncil defaults a missing source to "planner" rather than rejecting the
// requirement, because these round trip through LLM rewrites that drop
// provenance. Both planes must make the same call or one will refuse to load a
// document the other wrote.
func TestAMissingSourceDefaultsToPlanner(t *testing.T) {
	var r Requirement
	if err := json.Unmarshal([]byte(
		`{"id":"REQ-1","title":"t","description":"d","priority":"high"}`), &r); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if r.Source != SourcePlanner {
		t.Errorf("source = %q, want %q", r.Source, SourcePlanner)
	}
}

func TestAnUnknownPriorityIsRefused(t *testing.T) {
	var r Requirement
	err := json.Unmarshal([]byte(
		`{"id":"REQ-1","title":"t","description":"d","priority":"urgent"}`), &r)
	if err == nil {
		t.Fatal("an unknown priority decoded successfully")
	}
	if !strings.Contains(err.Error(), "REQ-1") {
		t.Errorf("the error must name the requirement: %v", err)
	}
}

func TestAnUnknownSourceIsRefusedRatherThanDefaulted(t *testing.T) {
	var r Requirement
	err := json.Unmarshal([]byte(
		`{"id":"REQ-1","title":"t","description":"d","priority":"low","source":"nobody"}`), &r)
	if err == nil {
		t.Fatal("an unknown source decoded successfully; the default is for an ABSENT key only")
	}
}

// A full requirement decodes with its criteria, and the nested criteria get the
// same defaulting.
func TestARequirementCarriesItsCriteria(t *testing.T) {
	var r Requirement
	body := `{"id":"REQ-1","title":"Add","description":"d","priority":"critical","source":"user",
	  "acceptance_criteria":[
	    {"id":"AC-1","description":"sums","verification_method":"unit_test"},
	    {"id":"AC-2","description":"errors","verification_method":"integration_test","required":false}]}`
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if r.Source != SourceUser {
		t.Errorf("source = %q, want user", r.Source)
	}
	if got := r.CriterionIDs(); len(got) != 2 || got[0] != "AC-1" || got[1] != "AC-2" {
		t.Fatalf("criterion ids = %v", got)
	}
	if !r.AcceptanceCriteria[0].Required {
		t.Error("AC-1 omitted `required` and must therefore be required")
	}
	if r.AcceptanceCriteria[1].Required {
		t.Error("AC-2 said required:false and must stay optional")
	}
}

// The gap DevCouncil's backfill exists to close: a criterion no task claims.
func TestUnownedCriteriaFindsWhatNoTaskIsAccountableFor(t *testing.T) {
	reqs := []Requirement{{
		ID: "REQ-1", Priority: PriorityHigh, Source: SourcePlanner,
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "AC-1", Method: VerifyUnitTest, Required: true},
			{ID: "AC-2", Method: VerifyUnitTest, Required: true},
			{ID: "AC-3", Method: VerifyManual, Required: true},
		},
	}}
	tasks := []Task{
		{ID: "T-1", AcceptanceCriterionIDs: []string{"AC-1"}},
		{ID: "T-2", AcceptanceCriterionIDs: []string{"AC-3"}},
	}

	got := UnownedCriteria(reqs, tasks)
	if len(got) != 1 || got[0] != "AC-2" {
		t.Fatalf("unowned = %v, want [AC-2]", got)
	}
}

// Every criterion owned means nothing is reported -- the check must not
// manufacture a gap where there is none.
func TestFullyOwnedCriteriaReportNothing(t *testing.T) {
	reqs := []Requirement{{
		ID: "REQ-1", Priority: PriorityLow,
		AcceptanceCriteria: []AcceptanceCriterion{{ID: "AC-1", Method: VerifyUnitTest, Required: true}},
	}}
	tasks := []Task{{ID: "T-1", AcceptanceCriterionIDs: []string{"AC-1"}}}
	if got := UnownedCriteria(reqs, tasks); len(got) != 0 {
		t.Fatalf("unowned = %v, want none", got)
	}
}

// With no tasks at all, every criterion is unowned. This is the shape that
// matters most: an empty plan must report every criterion as unbuilt rather
// than reporting nothing, which is what an ownership check written as an
// intersection would do.
func TestNoTasksMeansEveryCriterionIsUnowned(t *testing.T) {
	reqs := []Requirement{{
		ID: "REQ-1", Priority: PriorityLow,
		AcceptanceCriteria: []AcceptanceCriterion{
			{ID: "AC-1", Method: VerifyUnitTest, Required: true},
			{ID: "AC-2", Method: VerifyUnitTest, Required: true},
		},
	}}
	got := UnownedCriteria(reqs, nil)
	if len(got) != 2 {
		t.Fatalf("unowned = %v, want both criteria", got)
	}
}
