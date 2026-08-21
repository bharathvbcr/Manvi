package main

import (
	"reflect"
	"strings"
	"testing"

	"manvi/agent"
	"manvi/flags"
	"manvi/llm"
)

func noticeText(ns []outcomeNotice) string {
	var b strings.Builder
	for _, n := range ns {
		b.WriteString(n.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// TestACleanTurnSaysNothing is the baseline the rest depends on. A summary that
// always has something to say trains an operator to skip it.
func TestACleanTurnSaysNothing(t *testing.T) {
	if got := outcomeNotices(agent.Outcome{Steps: 3, ToolCalls: 2}, 500); len(got) != 0 {
		t.Fatalf("a clean turn produced %d notice(s): %q", len(got), noticeText(got))
	}
}

// TestEverySignalOnTheOutcomeIsReported is the guard against the drift this
// function was extracted to end: a signal recorded by the loop and mentioned by
// no face is indistinguishable, to everyone downstream, from a signal that
// never fired.
func TestEverySignalOnTheOutcomeIsReported(t *testing.T) {
	full := agent.Outcome{
		Steps: 40, ToolCalls: 12, Denied: 1, Qualified: 2, Repeated: 3, Stalled: 4,
		BudgetSpent: 120, NoProgressSteps: 5, Malformed: 6,
		TruncatedBySteps: true, TruncatedByTokens: true, FinalEmpty: true,
		Decoding: llm.DecodingReport{
			FallbackFormat: "xml", ReasoningReclassified: true, PrefillDisproved: true,
		},
		EffortFrom: "low", EffortTo: "high", EffortRaised: 2,
	}
	got := noticeText(outcomeNotices(full, 500))

	for _, want := range []string{
		"step ceiling", "output cap", "could not be read", "recovered from xml",
		"thinking tag", "refused by the gate", "not on the rules alone",
		"verbatim repeats", "changed nothing", "reasoning effort was raised",
		// A turn that produced nothing, and a declared prefill the server
		// contradicted. Both were compensated for in silence before, which is
		// exactly the drift this test exists to catch.
		"ended without an answer", "reasoning on its own channel",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("no notice mentioned %q; summary was:\n%s", want, got)
		}
	}
}

// TestTheCutOffAnswerIsReportedFirst — an operator who reads one line must read
// the one that says the answer in front of them is incomplete.
func TestTheCutOffAnswerIsReportedFirst(t *testing.T) {
	ns := outcomeNotices(agent.Outcome{
		FinalTruncated: true, TruncatedByTokens: true, Denied: 1, ToolCalls: 1,
	}, 500)
	if len(ns) == 0 {
		t.Fatal("a cut-off answer produced no notice at all")
	}
	if !strings.Contains(ns[0].Text, "cut off, not complete") {
		t.Fatalf("first notice was %q", ns[0].Text)
	}
	if len(ns[0].Degraded) == 0 {
		t.Error("a cut-off answer is a degradation, not a statistic")
	}
}

// TestARecoveredCapIsNotReportedAsACutOffAnswer keeps the two truncations
// apart. Saying both would tell an operator their answer is incomplete when it
// is whole, and a warning that cries wolf is one that gets ignored.
func TestARecoveredCapIsNotReportedAsACutOffAnswer(t *testing.T) {
	got := noticeText(outcomeNotices(agent.Outcome{TruncatedByTokens: true}, 500))
	if !strings.Contains(got, "recovered") {
		t.Errorf("a recovered cap was not mentioned at all: %q", got)
	}
	if strings.Contains(got, "cut off, not complete") {
		t.Errorf("a recovered turn was reported as a cut-off answer: %q", got)
	}

	both := noticeText(outcomeNotices(agent.Outcome{TruncatedByTokens: true, FinalTruncated: true}, 500))
	if strings.Contains(both, "recovered") {
		t.Errorf("a cut-off answer was also described as recovered: %q", both)
	}
}

// TestAQualifiedAllowIsNeverCalledARefusal is the reporting half of the gate
// fix: these calls ran and their writes landed.
func TestAQualifiedAllowIsNeverCalledARefusal(t *testing.T) {
	got := noticeText(outcomeNotices(agent.Outcome{Qualified: 1, ToolCalls: 1}, 500))
	if strings.Contains(got, "refused") {
		t.Fatalf("a qualified allow was reported as a refusal: %q", got)
	}
	if !strings.Contains(got, "not on the rules alone") {
		t.Fatalf("a qualified allow was not reported at all: %q", got)
	}
}

// TestAStaticEffortIsNeverReportedAsAnEscalation. Every turn carries a tier and
// most turns never move it; a summary that mentioned the tier regardless would
// be the noise that trains an operator to skip the summary.
func TestAStaticEffortIsNeverReportedAsAnEscalation(t *testing.T) {
	got := noticeText(outcomeNotices(agent.Outcome{
		Steps: 3, ToolCalls: 2, EffortFrom: "high", EffortTo: "high",
	}, 500))
	if got != "" {
		t.Fatalf("a turn that never changed tier produced: %q", got)
	}
}

// TestAnEscalationNamesBothEndsAndTheSettingThatAllowedIt. "effort was raised"
// on its own tells an operator nothing they can act on: the two tiers are what
// explain the turn's cost, and the flag is what they change if it was wrong.
func TestAnEscalationNamesBothEndsAndTheSettingThatAllowedIt(t *testing.T) {
	got := noticeText(outcomeNotices(agent.Outcome{
		Steps: 9, ToolCalls: 9, Stalled: 1, Repeated: 0, BudgetSpent: 15,
		EffortFrom: "low", EffortTo: "medium", EffortRaised: 1,
	}, 500))
	for _, want := range []string{`"low"`, `"medium"`, flags.LLMEffortCeiling} {
		if !strings.Contains(got, want) {
			t.Errorf("the escalation notice does not mention %s; summary was:\n%s", want, got)
		}
	}
}

// TestTheOutputCapNoticeDoesNotPrescribeRaisingTheCap.
//
// The notice used to end "raise llm.local.max_output_tokens". Hitting the cap
// has two opposite causes and this function cannot tell them apart: a genuinely
// long answer wants a bigger budget, and a model looping wants the cap left
// exactly where it is, because the cap is the only thing bounding it.
//
// Measured by following the old advice. Raising the cap from 16,384 to 32,768
// turned a 560-second looping step into a 979-second one producing 27,585
// tokens, and the turn died on its wall-clock timeout instead of its budget.
// Advice that makes the failure worse is worse than no advice.
func TestTheOutputCapNoticeDoesNotPrescribeRaisingTheCap(t *testing.T) {
	got := noticeText(outcomeNotices(agent.Outcome{FinalTruncated: true, Steps: 3}, 500))

	if !strings.Contains(got, "output cap") {
		t.Fatalf("the truncation notice stopped mentioning the output cap:\n%s", got)
	}
	// The fact, and both readings of it.
	for _, want := range []string{"cut off, not complete", "raise the cap", "longer loop"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice does not mention %q; it must state the fact and both readings:\n%s", want, got)
		}
	}
	// The bare imperative is what made it harmful: a reader who acts on the
	// first sentence must not be told to raise anything unconditionally.
	if strings.Contains(got, "not complete; raise") {
		t.Error("the notice still prescribes raising the cap unconditionally; on a looping turn " +
			"that removes the only bound on the loop")
	}
}

// TestContextOverflowIsReported. reportOverflow's own comment says history
// exceeding the budget after compaction "must not happen silently" — and it
// did. The event it appends has no case in ui.Project, which is the only bridge
// from the session log to either face, so the signal lived solely in the
// session file.
func TestContextOverflowIsReported(t *testing.T) {
	got := noticeText(outcomeNotices(agent.Outcome{ContextOverflowed: true, Steps: 4}, 500))
	if !strings.Contains(got, "context budget") {
		t.Fatalf("a turn whose history overflowed said nothing:\n%s", got)
	}
}

// TestTerminalStopReasonsAreReported. A refusal and an unmappable stop look
// identical to a completed turn on every other field: text arrived, no tool
// calls, nothing truncated, nothing empty. Outcome.StopReason was set every
// step and read by nothing outside tests.
func TestTerminalStopReasonsAreReported(t *testing.T) {
	for _, tc := range []struct {
		reason llm.StopReason
		want   string
	}{
		{llm.StopRefusal, "refused"},
		{llm.StopOther, "stop reason"},
	} {
		got := noticeText(outcomeNotices(agent.Outcome{StopReason: tc.reason, Steps: 2}, 500))
		if !strings.Contains(got, tc.want) {
			t.Errorf("StopReason %q produced no notice mentioning %q:\n%s", tc.reason, tc.want, got)
		}
	}
}

// The ordinary terminal reasons stay silent — a summary that always has
// something to say trains an operator to skip it.
func TestOrdinaryStopReasonsSayNothing(t *testing.T) {
	for _, reason := range []llm.StopReason{llm.StopEndTurn, llm.StopToolUse, ""} {
		if ns := outcomeNotices(agent.Outcome{StopReason: reason, Steps: 2}, 500); len(ns) != 0 {
			t.Errorf("StopReason %q produced %d notice(s): %q", reason, len(ns), noticeText(ns))
		}
	}
}

// TestEveryOutcomeFieldIsClassified is the guard that
// TestEverySignalOnTheOutcomeIsReported was named for but did not enforce.
//
// That test asserts a dozen hard-coded substrings against one hand-written
// Outcome literal. It uses no reflection, so it covers only what someone
// remembered to add — in both directions. Adding a field to agent.Outcome could
// not fail it, which is precisely how Outcome.StopReason came to be set on
// every step and read by nothing outside tests, and how ContextOverflowed was
// never surfaced at all despite reportOverflow's comment insisting it must not
// happen silently.
//
// This one walks the struct. Every field must be either reported by
// outcomeNotices or listed below with a reason it is not. A new field is a test
// failure until someone decides which it is — which is the whole invariant the
// original test's comment claims: "a signal recorded by the loop and mentioned
// by no face is indistinguishable, to everyone downstream, from a signal that
// never fired."
func TestEveryOutcomeFieldIsClassified(t *testing.T) {
	// Deliberately not a notice, each for a stated reason. Adding to this map
	// is a decision; leaving a field out of both is an oversight.
	silent := map[string]string{
		"Steps":      "a count, not a signal; it appears inside other notices and needs no line of its own",
		"Usage":      "rendered by both faces as a usage event, which is the right shape for a number",
		"Final":      "the answer itself; it is printed as the answer",
		"ToolCalls":  "the denominator in the Denied and Qualified notices, never reported alone",
		"EffortFrom": "reported as part of the EffortRaised notice",
		"EffortTo":   "reported as part of the EffortRaised notice",
	}
	// Fields whose notice fires from a different field being set. Listing them
	// keeps the map honest rather than letting them fall into `silent`.
	reportedVia := map[string]string{
		"NoProgressSteps": "TruncatedBySteps",
		"BudgetSpent":     "Repeated/Stalled",
	}

	typ := reflect.TypeOf(agent.Outcome{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if _, ok := silent[name]; ok {
			continue
		}
		if _, ok := reportedVia[name]; ok {
			continue
		}
		if !fieldProducesANotice(t, name) {
			t.Errorf("Outcome.%s is set by the loop and produces no notice, and is not listed as "+
				"deliberately silent. Either report it or say in the map above why it is not reported — "+
				"a signal nobody sees is indistinguishable from one that never fired", name)
		}
	}
}

// fieldProducesANotice sets one field to a non-zero value and reports whether
// outcomeNotices says anything at all about the resulting outcome.
func fieldProducesANotice(t *testing.T, field string) bool {
	t.Helper()
	var o agent.Outcome
	v := reflect.ValueOf(&o).Elem().FieldByName(field)
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int64:
		v.SetInt(3)
	case reflect.String:
		v.SetString(llmStopForTest(field))
	case reflect.Struct:
		// Decoding: any one flag is enough to make Clean() false.
		if field == "Decoding" {
			o.Decoding = llm.DecodingReport{PrefillDisproved: true}
		}
	default:
		t.Fatalf("Outcome.%s has kind %s, which this guard does not know how to set", field, v.Kind())
	}
	return len(outcomeNotices(o, 500)) > 0
}

// llmStopForTest gives StopReason a value that should be reported. Any other
// string field would take the empty-ish path and is caught by the caller.
func llmStopForTest(field string) string {
	if field == "StopReason" {
		return string(llm.StopRefusal)
	}
	return "set"
}
