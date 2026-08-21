package main

import (
	"errors"
	"testing"

	"manvi/agent"
	"manvi/llm"
)

// TestEveryDegradedOutcomeCarriesANonZeroStatus.
//
// The rule this enforces is the harness's own: a check that did not run must
// never report the same result as one that ran and passed. An exit status is a
// check's result, and it is the only one a benchmark or a CI step reads.
//
// A turn whose stream died mid-answer used to exit 0 — the same status a
// completed turn gets — while printing "the answer above may be incomplete and
// nothing else will say so" to stderr. That notice was right about being the
// only signal and wrong about it being enough.
func TestEveryDegradedOutcomeCarriesANonZeroStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome agent.Outcome
		want    error
	}{
		{"cut off by the output cap", agent.Outcome{FinalTruncated: true}, errOutputCap},
		{"ended by the step ceiling", agent.Outcome{TruncatedBySteps: true}, errTruncated},
		{"no answer at all", agent.Outcome{FinalEmpty: true}, errNoAnswer},
		{"stream ended on an unmapped stop", agent.Outcome{StopReason: llm.StopOther}, errUnfinished},
		{"the model refused", agent.Outcome{StopReason: llm.StopRefusal}, errUnfinished},
	} {
		got := outcomeStatus(tc.outcome)
		if got == nil {
			t.Errorf("%s: exited 0 — indistinguishable from a turn that finished", tc.name)
			continue
		}
		if !errors.Is(got, tc.want) {
			t.Errorf("%s: status %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestAFinishedTurnExitsZero is the other half: a status that is always
// non-zero is a status nobody can act on.
func TestAFinishedTurnExitsZero(t *testing.T) {
	for _, reason := range []llm.StopReason{llm.StopEndTurn, llm.StopToolUse, llm.StopMaxTokens} {
		if err := outcomeStatus(agent.Outcome{StopReason: reason, Steps: 3}); err != nil {
			t.Errorf("StopReason %q exited non-zero: %v", reason, err)
		}
	}
}

// TestTheStatusAndTheNoticeAgree.
//
// outcomeNotices and outcomeStatus are two renderings of one judgement, and
// they had already drifted: every condition below marked its notice Degraded
// while only three of the four carried a status. Anything the summary calls
// degraded must also be something a script can see.
func TestTheStatusAndTheNoticeAgree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome agent.Outcome
	}{
		{"output cap", agent.Outcome{FinalTruncated: true}},
		{"step ceiling", agent.Outcome{TruncatedBySteps: true}},
		{"no answer", agent.Outcome{FinalEmpty: true}},
		{"unmapped stop", agent.Outcome{StopReason: llm.StopOther}},
		{"refusal", agent.Outcome{StopReason: llm.StopRefusal}},
	} {
		var degraded bool
		for _, n := range outcomeNotices(tc.outcome, 500) {
			if len(n.Degraded) > 0 {
				degraded = true
			}
		}
		if degraded && outcomeStatus(tc.outcome) == nil {
			t.Errorf("%s: the summary calls this degraded and the exit status calls it success", tc.name)
		}
	}
}
