package ui

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func askRequest(multi bool) Request {
	return Request{
		ID: "QUESTION-0001", Rule: "pair.questions.enabled", Severity: "ask",
		Subject: "question", Reason: "Which database backend should we configure?",
		Grantable:   true,
		Choices:     []string{"SQLite WAL mode", "PostgreSQL 16", "MySQL 8"},
		MultiSelect: multi,
	}
}

// TestAnAllowCarryingNoChoiceIsNotAnAnswer is the guard the whole seam rests
// on. Allow alone is what a decision that reached nobody still carries, and a
// caller reading it would record an empty selection as a human's answer.
func TestAnAllowCarryingNoChoiceIsNotAnAnswer(t *testing.T) {
	if (Decision{Allow: true, Reason: "cleared"}).Answered() {
		t.Error("an allow with nothing chosen was reported as an answer")
	}
	if (Decision{Allow: true, WriteIn: "   "}).Answered() {
		t.Error("a blank write-in was reported as an answer")
	}
	if !(Decision{Allow: true, Chosen: []string{"a"}}).Answered() {
		t.Error("a chosen option was not reported as an answer")
	}
	if (Decision{Allow: false, Chosen: []string{"a"}}).Answered() {
		t.Error("a refused decision was reported as an answer")
	}
}

// TestAStandingApprovalCannotAnswerAQuestion keeps an unattended run from
// answering on a human's behalf through the rule list. A rule list says which
// blocks may be cleared; it cannot say which of four options a person wanted.
func TestAStandingApprovalCannotAnswerAQuestion(t *testing.T) {
	a := AutoApprover{
		Rules:  map[string]bool{"pair.questions.enabled": true},
		Reason: "standing approval for this hour",
	}
	d, err := a.Approve(context.Background(), askRequest(false))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if d.Answered() {
		t.Fatalf("a standing approval answered a question: %#v", d)
	}
	if d.Allow {
		t.Errorf("a standing approval allowed a question: %q", d.Reason)
	}
}

// TestDenyAllAnswersNothing is the headless default: nobody is there, so
// nothing is answered.
func TestDenyAllAnswersNothing(t *testing.T) {
	d, err := DenyAll{}.Approve(context.Background(), askRequest(false))
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if d.Answered() {
		t.Fatalf("an unattended run answered a question: %#v", d)
	}
}

func promptWith(t *testing.T, input string, req Request) (Decision, string) {
	t.Helper()
	var out bytes.Buffer
	p := NewPrompter(strings.NewReader(input), &out, nil)
	d, err := p.Approve(context.Background(), req)
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	return d, out.String()
}

func TestPrompterPutsTheOptionsToTheOperator(t *testing.T) {
	d, out := promptWith(t, "2\n", askRequest(false))
	if !strings.Contains(out, "Which database backend should we configure?") {
		t.Errorf("the question was not shown:\n%s", out)
	}
	if !strings.Contains(out, "2) PostgreSQL 16") {
		t.Errorf("the options were not shown:\n%s", out)
	}
	if strings.Contains(out, "[y/N]") {
		t.Errorf("a question was put as an allow/deny prompt:\n%s", out)
	}
	if !d.Answered() || len(d.Chosen) != 1 || d.Chosen[0] != "PostgreSQL 16" {
		t.Fatalf("decision = %#v", d)
	}
	if d.By != "human" {
		t.Errorf("By = %q, want \"human\"", d.By)
	}
}

func TestPrompterTakesFreeTextAsAWriteIn(t *testing.T) {
	d, _ := promptWith(t, "none of these; use DuckDB\n", askRequest(false))
	if !d.Answered() || d.WriteIn != "none of these; use DuckDB" {
		t.Fatalf("decision = %#v", d)
	}
	if len(d.Chosen) != 0 {
		t.Errorf("a write-in was also recorded as a listed choice: %#v", d.Chosen)
	}
}

func TestPrompterTakesSeveralAnswersOnlyWhenTheQuestionAsksForThem(t *testing.T) {
	d, _ := promptWith(t, "1,3\n", askRequest(true))
	if !d.Answered() || len(d.Chosen) != 2 {
		t.Fatalf("multi-select decision = %#v", d)
	}

	// The same input against a single-select question is refused rather than
	// silently narrowed to the first: picking one of two answers a person gave
	// is the harness deciding, not the person.
	d, _ = promptWith(t, "1,3\n", askRequest(false))
	if d.Answered() {
		t.Fatalf("a single-select question accepted two answers: %#v", d)
	}
}

func TestPrompterRefusesAnOptionThatWasNotOffered(t *testing.T) {
	d, _ := promptWith(t, "9\n", askRequest(false))
	if d.Answered() {
		t.Fatalf("an option that was never offered was accepted: %#v", d)
	}
	if !strings.Contains(d.Reason, "not one of the 3 offered") {
		t.Errorf("Reason = %q", d.Reason)
	}
}

// TestAnUnreadableQuestionIsUnanswered: closed input is not an answer, for the
// same reason an unanswerable approval is not an allow.
func TestAnUnreadableQuestionIsUnanswered(t *testing.T) {
	d, _ := promptWith(t, "", askRequest(false))
	if d.Answered() || d.Allow {
		t.Fatalf("a question nobody could answer came back answered: %#v", d)
	}
}
