package devcouncil

import (
	"context"
	"testing"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// An escalation carries an id of its own, and no two requests share one. The
// headless approver binds a host's decision line to the request through it;
// before this, escalations had no id and every question call restarted at
// QUESTION-0001, so there was nothing for a line to be bound to.
func TestEscalationRequestsCarryAUniqueID(t *testing.T) {
	approver := &zfixtoolsApprover{}
	f := newFixture(t)
	f.reg.deps.Approver = approver

	decision := policy.Decision{
		Action: policy.Deny, Rule: policy.RuleCommandNotAllowed,
		Severity: policy.Soft, Target: "cat seed.txt",
	}
	for i := 0; i < 2; i++ {
		f.reg.escalate(context.Background(), decision, "cat seed.txt")
	}
	if len(approver.asked) != 2 {
		t.Fatalf("asked %d times, want 2", len(approver.asked))
	}
	first, second := approver.asked[0].ID, approver.asked[1].ID
	if first == "" || second == "" || first == second {
		t.Fatalf("escalation ids = %q, %q; want two distinct non-empty ids", first, second)
	}
}

type idRecordingApprover struct{ ids []string }

func (a *idRecordingApprover) Approve(_ context.Context, req ui.Request) (ui.Decision, error) {
	a.ids = append(a.ids, req.ID)
	return ui.Decision{Allow: true, Chosen: req.Choices[:1], By: "human"}, nil
}

func TestQuestionIDsDoNotRepeatAcrossCallsOrWithinOne(t *testing.T) {
	approver := &idRecordingApprover{}
	asker := ApproverAsker{Approver: approver}
	questions := []Question{
		{Question: "one?", Options: []string{"a", "b"}},
		{Question: "two?", Options: []string{"a", "b"}},
	}
	for i := 0; i < 2; i++ {
		if _, err := asker.AskQuestions(context.Background(), questions); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for _, id := range approver.ids {
		if id == "" || id == "QUESTION-0001" || seen[id] {
			t.Fatalf("question ids = %q; want four distinct ids, none of them the old per-call QUESTION-000N", approver.ids)
		}
		seen[id] = true
	}
	if len(seen) != 4 {
		t.Fatalf("question ids = %q; want 4", approver.ids)
	}
}
