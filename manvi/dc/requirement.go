package dc

import (
	"encoding/json"
	"fmt"
)

// Requirement and AcceptanceCriterion are the half of DevCouncil's task model
// this harness could not previously represent.
//
// A Task carries RequirementIDs and AcceptanceCriterionIDs, which are only
// identifiers; these are what they point at. Without them the harness can hold
// a task and enforce its file scope, but it cannot answer the two questions
// DevCouncil exists to answer — why does this task exist, and what would prove
// it is done.
//
// The wire shape is DevCouncil's, field for field, because these values round
// trip through the same artifacts and the same LLM calls on both sides. Two
// spellings of one model is how a plan written by one plane becomes unreadable
// to the other.

// VerificationMethod is how an acceptance criterion is meant to be proved.
//
// It is not a free string: the value decides which gate is allowed to discharge
// the criterion, and a typo that read as an unknown method would leave a
// criterion no gate claims, which is the silent-pass shape this harness refuses
// everywhere else.
type VerificationMethod string

const (
	VerifyUnitTest        VerificationMethod = "unit_test"
	VerifyIntegrationTest VerificationMethod = "integration_test"
	VerifyManual          VerificationMethod = "manual"
	VerifyStaticCheck     VerificationMethod = "static_check"
	VerifyLLMReview       VerificationMethod = "llm_review"
)

func (m VerificationMethod) valid() bool {
	switch m {
	case VerifyUnitTest, VerifyIntegrationTest, VerifyManual, VerifyStaticCheck, VerifyLLMReview:
		return true
	}
	return false
}

// Priority orders requirements. DevCouncil's four levels, no more: an
// unrecognised level would sort in an arbitrary place.
type Priority string

const (
	PriorityLow      Priority = "low"
	PriorityMedium   Priority = "medium"
	PriorityHigh     Priority = "high"
	PriorityCritical Priority = "critical"
)

func (p Priority) valid() bool {
	switch p {
	case PriorityLow, PriorityMedium, PriorityHigh, PriorityCritical:
		return true
	}
	return false
}

// Source records who authored a requirement. It matters for trust: a
// requirement the user stated and one a critic inferred are not equally
// authoritative, and collapsing them loses the only provenance there is.
type Source string

const (
	SourceUser    Source = "user"
	SourcePlanner Source = "planner"
	SourceCritic  Source = "critic"
	SourceArbiter Source = "arbiter"
)

func (s Source) valid() bool {
	switch s {
	case SourceUser, SourcePlanner, SourceCritic, SourceArbiter:
		return true
	}
	return false
}

// AcceptanceCriterion is one checkable statement about the finished work.
type AcceptanceCriterion struct {
	ID          string             `json:"id"`
	Description string             `json:"description"`
	Method      VerificationMethod `json:"verification_method"`
	// Required defaults to true when absent, matching DevCouncil.
	//
	// This default is load-bearing and is the reason this type decodes by hand.
	// Go zeroes a bool to false, so a criterion serialised without the key --
	// which is every criterion a producer omitted it from, and pydantic omits
	// defaults readily -- would silently arrive as optional. The criterion
	// would still be listed, still look checked, and quietly stop being
	// something the work has to satisfy.
	Required bool `json:"required"`
}

// acceptanceWire decodes with Required as a pointer so an absent key is
// distinguishable from an explicit false.
type acceptanceWire struct {
	ID          string             `json:"id"`
	Description string             `json:"description"`
	Method      VerificationMethod `json:"verification_method"`
	Required    *bool              `json:"required"`
}

func (a *AcceptanceCriterion) UnmarshalJSON(data []byte) error {
	var w acceptanceWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.ID == "" {
		return fmt.Errorf("acceptance criterion has no id")
	}
	if !w.Method.valid() {
		return fmt.Errorf(
			"acceptance criterion %s has verification_method %q, which no gate can discharge "+
				"(want one of unit_test, integration_test, manual, static_check, llm_review)",
			w.ID, w.Method)
	}
	a.ID = w.ID
	a.Description = w.Description
	a.Method = w.Method
	a.Required = w.Required == nil || *w.Required
	return nil
}

// Requirement is a thing the finished system must do, and the criteria that
// would show it does.
type Requirement struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Priority    Priority `json:"priority"`
	// Source defaults to "planner" when absent, matching DevCouncil.
	//
	// Requirements come back through LLM round trips -- arbiter and critic
	// rewrites -- where weaker models drop provenance metadata. DevCouncil
	// chose not to invalidate an otherwise sound requirement over it, and this
	// side has to make the same choice or the two planes disagree about which
	// documents are loadable.
	Source             Source                `json:"source"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
}

type requirementWire struct {
	ID                 string                `json:"id"`
	Title              string                `json:"title"`
	Description        string                `json:"description"`
	Priority           Priority              `json:"priority"`
	Source             *Source               `json:"source"`
	AcceptanceCriteria []AcceptanceCriterion `json:"acceptance_criteria"`
}

func (r *Requirement) UnmarshalJSON(data []byte) error {
	var w requirementWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	if w.ID == "" {
		return fmt.Errorf("requirement has no id")
	}
	if !w.Priority.valid() {
		return fmt.Errorf(
			"requirement %s has priority %q (want one of low, medium, high, critical)",
			w.ID, w.Priority)
	}
	source := SourcePlanner
	if w.Source != nil {
		if !(*w.Source).valid() {
			return fmt.Errorf(
				"requirement %s has source %q (want one of user, planner, critic, arbiter)",
				w.ID, *w.Source)
		}
		source = *w.Source
	}
	r.ID = w.ID
	r.Title = w.Title
	r.Description = w.Description
	r.Priority = w.Priority
	r.Source = source
	r.AcceptanceCriteria = w.AcceptanceCriteria
	return nil
}

// CriterionIDs is every acceptance criterion this requirement declares.
func (r *Requirement) CriterionIDs() []string {
	ids := make([]string, 0, len(r.AcceptanceCriteria))
	for _, ac := range r.AcceptanceCriteria {
		ids = append(ids, ac.ID)
	}
	return ids
}

// UnownedCriteria returns the criteria of these requirements that no task in
// tasks is accountable for.
//
// This is the question DevCouncil's planner answers by backfilling: the spec
// elaborates edge-case and error criteria, and a planner may link only some of
// them to tasks, silently dropping the rest from per-criterion verification. A
// criterion nobody owns is a behaviour nobody is accountable for building --
// and, crucially, nothing downstream ever reports it as missing, because the
// gates only ever check the criteria a task claims.
//
// Returned in the order the requirements declare them, so a report reads in
// spec order rather than in map order.
func UnownedCriteria(reqs []Requirement, tasks []Task) []string {
	owned := make(map[string]bool)
	for _, t := range tasks {
		for _, id := range t.AcceptanceCriterionIDs {
			owned[id] = true
		}
	}
	var unowned []string
	for i := range reqs {
		for _, ac := range reqs[i].AcceptanceCriteria {
			if !owned[ac.ID] {
				unowned = append(unowned, ac.ID)
			}
		}
	}
	return unowned
}
