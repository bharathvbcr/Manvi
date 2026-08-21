package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"manvi/tools"
	"manvi/ui"
)

// answeringApprover records what it was asked and answers with a fixed choice.
type answeringApprover struct {
	seen []ui.Request
}

func (a *answeringApprover) Approve(ctx context.Context, req ui.Request) (ui.Decision, error) {
	a.seen = append(a.seen, req)
	if !req.IsQuestion() {
		return ui.Decision{Allow: false, Reason: "not under test"}, nil
	}
	return ui.Decision{
		Allow: true, By: "human", Reason: "chosen by the operator",
		Chosen: []string{req.Choices[len(req.Choices)-1]},
	}, nil
}

// TestTheAttendedToolSurfacePutsQuestionsToTheOperator is the wiring test for
// the defect this file was added for.
//
// Nothing in the harness ever set QuestionAsker, so devcouncil_ask_question
// took its unattended branch on every call ever made — including here, in the
// attended path, where an operator was sitting at a terminal while the harness
// picked an option on their behalf and reported it as answered. The check is
// that a surface built with an approver attached reaches that approver.
func TestTheAttendedToolSurfacePutsQuestionsToTheOperator(t *testing.T) {
	ap := &answeringApprover{}
	_, pipeline, err := nativeToolsWith(newTestRegistry(t), ap)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}

	args, err := json.Marshal(map[string]any{
		"questions": []map[string]any{{
			"question": "Which database backend should we configure?",
			"options":  []string{"SQLite WAL mode", "PostgreSQL 16"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := pipeline.Run(context.Background(), tools.Call{
		ID: "c1", Name: "devcouncil_ask_question", Arguments: args,
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	if len(ap.seen) != 1 {
		t.Fatalf("the operator was asked %d times, want 1: the question never reached them", len(ap.seen))
	}
	if ap.seen[0].Reason != "Which database backend should we configure?" {
		t.Errorf("the question text did not reach the operator: %q", ap.seen[0].Reason)
	}

	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, res.Text)
	}
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

// TestAnUnattendedToolSurfaceAttachesNoAsker keeps the CLI and headless cases
// honest: with nobody attached, the tool must report that nobody was asked
// rather than reporting an answer.
func TestAnUnattendedToolSurfaceAttachesNoAsker(t *testing.T) {
	if questionAsker(nil) != nil {
		t.Fatal("a nil approver produced a non-nil asker; every question would then be refused")
	}

	_, pipeline, err := nativeToolsWith(newTestRegistry(t), nil)
	if err != nil {
		t.Skipf("the native tool surface could not be built here: %v", err)
	}
	args, err := json.Marshal(map[string]any{
		"questions": []map[string]any{{
			"question": "Which one?",
			"options":  []string{"(Recommended) a", "b"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := pipeline.Run(context.Background(), tools.Call{
		ID: "c1", Name: "devcouncil_ask_question", Arguments: args,
	})
	if res.IsError {
		t.Fatalf("ask question failed: %s", res.Text)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(res.Text), &payload); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, res.Text)
	}
	if payload["answered"] != false {
		t.Errorf("answered = %v, want false: nobody was attached to ask", payload["answered"])
	}
	if _, ok := payload["assumed_defaults"]; !ok {
		t.Errorf("the assumed defaults are not reported as assumptions: %s", res.Text)
	}
	// The auto-resolve behaviour itself is wanted; only its self-report was wrong.
	if !strings.Contains(res.Text, "(Recommended) a") {
		t.Errorf("the recommended default was not assumed: %s", res.Text)
	}
}
