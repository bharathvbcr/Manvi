package ui

import (
	"testing"
)

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
