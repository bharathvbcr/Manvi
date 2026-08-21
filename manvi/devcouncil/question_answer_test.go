package devcouncil

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"manvi/flags"
	"manvi/ui"
)

// decodeResult unmarshals a tool result's JSON payload, failing the test if it
// is not JSON. Every assertion below reads the payload rather than substring
// matching the text, because the distinction under test — "a human answered"
// versus "nobody answered and a default was assumed" — is a field, not a
// phrase, and a substring match would pass on a payload that says the opposite.
func decodeResult(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out
}

// TestAQuestionNobodyAnsweredIsNotReportedAsAnswered is the whole point of the
// unattended branch. The tool used to return answered:true for a question no
// human ever saw, which is a report of work that did not happen.
func TestAQuestionNobodyAnsweredIsNotReportedAsAnswered(t *testing.T) {
	f := newFixture(t)

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Which database backend should we configure?",
			"options":  []string{"(Recommended) SQLite WAL mode", "PostgreSQL 16"},
		}},
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	payload := decodeResult(t, res.Text)

	if payload["answered"] != false {
		t.Errorf("answered = %v, want false: nobody was asked", payload["answered"])
	}
	if _, ok := payload["answers"]; ok {
		t.Errorf("payload carries an %q key for a question nobody answered: %s", "answers", res.Text)
	}
	if _, ok := payload["assumed_defaults"]; !ok {
		t.Errorf("payload does not carry the assumed defaults it acted on: %s", res.Text)
	}
	if by, _ := payload["answered_by"].(string); by != "none" {
		t.Errorf("answered_by = %q, want \"none\"", by)
	}
	// The recommended default is still selected — the auto-resolve behaviour is
	// wanted, only its self-report was dishonest.
	if !strings.Contains(res.Text, "(Recommended) SQLite WAL mode") {
		t.Errorf("expected the recommended option to be the assumed default: %s", res.Text)
	}
}

// TestAHumanAnsweredQuestionSaysAHumanAnsweredIt is the other half: the caller
// must be able to tell the two apart from the result alone.
func TestAHumanAnsweredQuestionSaysAHumanAnsweredIt(t *testing.T) {
	asker := &testQuestionAsker{}
	f := newFixture(t)
	f.reg.deps.QuestionAsker = asker

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Select CSS methodology",
			"options":  []string{"Vanilla CSS Tokens", "TailwindCSS"},
		}},
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	if !asker.answered {
		t.Fatalf("the attached asker was never called")
	}
	payload := decodeResult(t, res.Text)
	if payload["answered"] != true {
		t.Errorf("answered = %v, want true", payload["answered"])
	}
	if by, _ := payload["answered_by"].(string); by != "human" {
		t.Errorf("answered_by = %q, want \"human\"", by)
	}
	if _, ok := payload["assumed_defaults"]; ok {
		t.Errorf("a human-answered question must not report assumed defaults: %s", res.Text)
	}
}

// shortAsker returns fewer answers than it was asked for.
type shortAsker struct{}

func (shortAsker) AskQuestions(ctx context.Context, questions []Question) ([]QuestionAnswer, error) {
	return nil, nil
}

// TestAnAskerThatAnswersNothingIsAnErrorNotAnAnswer keeps the seam failing
// closed: an asker that comes back empty has not answered anything, and
// reporting that as answered:true is the same lie by another route.
func TestAnAskerThatAnswersNothingIsAnErrorNotAnAnswer(t *testing.T) {
	f := newFixture(t)
	f.reg.deps.QuestionAsker = shortAsker{}

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Which one?",
			"options":  []string{"a", "b"},
		}},
	})
	if !res.IsError {
		t.Fatalf("an asker that answered nothing was reported as success: %s", res.Text)
	}
}

// emptyAsker answers every question with nothing selected and nothing written.
type emptyAsker struct{}

func (emptyAsker) AskQuestions(ctx context.Context, questions []Question) ([]QuestionAnswer, error) {
	out := make([]QuestionAnswer, len(questions))
	for i, q := range questions {
		out[i] = QuestionAnswer{Question: q.Question}
	}
	return out, nil
}

func TestAnAnswerWithNothingChosenIsAnError(t *testing.T) {
	f := newFixture(t)
	f.reg.deps.QuestionAsker = emptyAsker{}

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Which one?",
			"options":  []string{"a", "b"},
		}},
	})
	if !res.IsError {
		t.Fatalf("an empty selection was reported as an answer: %s", res.Text)
	}
}

// failingAsker reports that it could not put the question to anyone.
type failingAsker struct{}

func (failingAsker) AskQuestions(ctx context.Context, questions []Question) ([]QuestionAnswer, error) {
	return nil, errors.New("the operator's session went away")
}

func TestAnAskerErrorIsAnErrorNotAnAutoResolve(t *testing.T) {
	f := newFixture(t)
	f.reg.deps.QuestionAsker = failingAsker{}

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Which one?",
			"options":  []string{"a", "b"},
		}},
	})
	if !res.IsError {
		t.Fatalf("a failed ask fell through to a default and reported it: %s", res.Text)
	}
}

// TestPairQuestionsDisabledDoesNotPutQuestionsToAHuman makes the flag
// load-bearing: with pairing off, an attached asker is not consulted and the
// result says the run did not pair.
func TestPairQuestionsDisabledDoesNotPutQuestionsToAHuman(t *testing.T) {
	asker := &testQuestionAsker{}
	f := newFixtureWith(t, map[string]string{
		flags.HarnessPosture:       flags.PostureStrict,
		flags.PairQuestionsEnabled: "false",
	})
	f.reg.deps.QuestionAsker = asker

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Which one?",
			"options":  []string{"a", "b"},
		}},
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	if asker.answered {
		t.Errorf("pairing is disabled but the question was still put to a human")
	}
	payload := decodeResult(t, res.Text)
	if payload["answered"] != false {
		t.Errorf("answered = %v, want false", payload["answered"])
	}
	note, _ := payload["note"].(string)
	if !strings.Contains(note, flags.PairQuestionsEnabled) {
		t.Errorf("note does not name the setting that suppressed the question: %q", note)
	}
}

// askingApprover records the requests put to it and answers with a fixed
// decision, so a test can check what a question looks like by the time it
// reaches the seam the operator sits behind.
type askingApprover struct {
	seen     []ui.Request
	decision ui.Decision
}

func (a *askingApprover) Approve(ctx context.Context, req ui.Request) (ui.Decision, error) {
	a.seen = append(a.seen, req)
	return a.decision, nil
}

// TestQuestionsGoThroughTheRunsApprovalSeam is the wiring: pairing reaches the
// operator through ui.Approver — the same seam the write gate escalates a soft
// block through — rather than through a second prompting mechanism beside it.
func TestQuestionsGoThroughTheRunsApprovalSeam(t *testing.T) {
	ap := &askingApprover{decision: ui.Decision{
		Allow: true, By: "human", Reason: "chosen by the operator",
		Chosen: []string{"PostgreSQL 16"},
	}}
	f := newFixture(t)
	f.reg.deps.QuestionAsker = ApproverAsker{Approver: ap}

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question":        "Which database backend?",
			"options":         []string{"SQLite", "PostgreSQL 16"},
			"is_multi_select": true,
		}},
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	if len(ap.seen) != 1 {
		t.Fatalf("the approver saw %d requests, want 1", len(ap.seen))
	}
	req := ap.seen[0]
	if !req.IsQuestion() {
		t.Errorf("the request did not carry the options: %#v", req)
	}
	if !req.MultiSelect {
		t.Errorf("is_multi_select did not reach the operator: %#v", req)
	}
	if req.Reason != "Which database backend?" {
		t.Errorf("the question text did not reach the operator: %q", req.Reason)
	}
	if !req.Grantable {
		t.Errorf("the question was raised as unanswerable: %#v", req)
	}

	payload := decodeResult(t, res.Text)
	if payload["answered"] != true {
		t.Errorf("answered = %v, want true", payload["answered"])
	}
	if by, _ := payload["answered_by"].(string); by != "human" {
		t.Errorf("answered_by = %q, want \"human\"", by)
	}
	if !strings.Contains(res.Text, "PostgreSQL 16") {
		t.Errorf("the operator's choice is missing from the result: %s", res.Text)
	}
}

// TestADismissedQuestionIsReportedUnanswered covers the operator who is there
// and says nothing. It must not read like the operator agreed to the default.
func TestADismissedQuestionIsReportedUnanswered(t *testing.T) {
	ap := &askingApprover{decision: ui.Decision{
		Allow: false, Reason: "dismissed by the operator without answering",
	}}
	f := newFixture(t)
	f.reg.deps.QuestionAsker = ApproverAsker{Approver: ap}

	res := f.call("devcouncil_ask_question", map[string]any{
		"questions": []map[string]any{{
			"question": "Which one?",
			"options":  []string{"a", "b"},
		}},
	})
	if res.IsError {
		t.Fatalf("a dismissed question was an error: %s", res.Text)
	}
	payload := decodeResult(t, res.Text)
	if payload["answered"] != false {
		t.Errorf("answered = %v, want false", payload["answered"])
	}
	if note, _ := payload["note"].(string); !strings.Contains(note, "dismissed") {
		t.Errorf("note does not say the operator dismissed it: %q", note)
	}
}

// TestAnAllowCarryingNoChoiceIsNotTreatedAsAnAnswer is the seam's own trap: a
// decision that says Allow but carries nothing chosen is what an approver
// returns when it could not put the question to anybody.
func TestAnAllowCarryingNoChoiceIsNotTreatedAsAnAnswer(t *testing.T) {
	ap := &askingApprover{decision: ui.Decision{Allow: true, By: "human", Reason: "cleared"}}
	_, err := ApproverAsker{Approver: ap}.AskQuestions(context.Background(), []Question{
		{Question: "Which one?", Options: []string{"a", "b"}},
	})
	if !errors.Is(err, ErrQuestionDeclined) {
		t.Fatalf("err = %v, want a declined question", err)
	}
}

func TestAnAskerWithNoApproverRefusesRatherThanAnswering(t *testing.T) {
	_, err := ApproverAsker{}.AskQuestions(context.Background(), []Question{
		{Question: "Which one?", Options: []string{"a", "b"}},
	})
	if !errors.Is(err, ui.ErrNoApprover) {
		t.Fatalf("err = %v, want ui.ErrNoApprover", err)
	}
}
