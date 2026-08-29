package main

import (
	"fmt"

	"manvi/agent"
	"manvi/flags"
	"manvi/llm"
)

// outcomeNotice is one thing worth saying about a finished turn.
//
// Degraded is non-empty when the turn did not do what its output looks like it
// did, which is the distinction the faces render differently: a count is
// information, a degradation is a warning.
type outcomeNotice struct {
	Text     string
	Degraded []string
}

// outcomeNotices is the single answer to "what should be said about this
// turn", shared by every face.
//
// It exists because there were two hand-rolled copies of that answer — one in
// the headless summary, one in the TUI — and they had drifted apart. The TUI
// never mentioned repeats, stalls, or the output cap; neither mentioned tool
// calls the adapter could not read, or the compensations it made to read a
// response at all, though the loop had been recording both all along and
// Outcome.Decoding is documented as existing precisely so a misconfigured
// server "is visible in the outcome". A signal added to Outcome and wired into
// one face out of two is the shape of every bug this function removes: there is
// now one place to add it, and both faces get it.
//
// Ordering is deliberate: what invalidates the result first, then what explains
// its cost.
func outcomeNotices(o agent.Outcome, maxSteps int) []outcomeNotice {
	var out []outcomeNotice

	// Ahead of everything else, because it is the one notice that contradicts
	// the turn's own appearance. A natural stop with prose and no tool calls
	// looks finished from every other signal here; if the check that ran over
	// its changes failed, saying so anywhere but first invites the reader to
	// have already decided.
	switch o.Sensor {
	case agent.SensorNone, agent.SensorSkipped, agent.SensorPassed:
		// Nothing to say. No check was owed, none was attempted, or one ran
		// and was satisfied — and a line on every clean turn is a line an
		// operator learns to skip, which is how the failing one gets skipped
		// too.
	case agent.SensorFailed:
		text := "the end-of-turn check failed on this turn's changes — the work is not verified"
		if o.BouncesExhausted {
			text += ", and the turn ended with the check still failing after " +
				fmt.Sprintf("%d attempt(s) to put it right", o.Bounces)
		}
		out = append(out, outcomeNotice{
			Text:     text,
			Degraded: []string{"end-of-turn check failed"},
		})
	case agent.SensorDegraded:
		// Never folded into the pass. A check that could not run is the exact
		// state that must not read like a check that ran and found nothing:
		// that equivalence is how "verified" comes to mean "unexamined".
		out = append(out, outcomeNotice{
			Text: "the end-of-turn check could not run over this turn's changes, so nothing here " +
				"is evidence that they are sound",
			Degraded: []string{"end-of-turn check degraded"},
		})
	default:
		// A verdict this build does not recognise. Reported rather than
		// ignored, and reported as a degradation: a value nobody anticipated
		// is not evidence that anything passed, and the default that treats it
		// as one is how a renamed constant silently disarms a gate.
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("the end-of-turn check returned %q, which this build does not "+
				"recognise; treat this turn as unverified", string(o.Sensor)),
			Degraded: []string{"end-of-turn check returned an unrecognised verdict"},
		})
	}

	// A turn that had to be sent back says so even when it eventually passed:
	// the operator asked for one answer and paid for three, and the reason is
	// not otherwise visible anywhere in the transcript.
	if o.Bounces > 0 || o.BouncesExhausted {
		notice := outcomeNotice{
			Text: fmt.Sprintf("the end-of-turn check sent this turn back %d time(s) before it closed",
				o.Bounces),
		}
		if o.BouncesExhausted {
			notice.Text = fmt.Sprintf(
				"the end-of-turn check was still not satisfied after %d attempt(s), which is the "+
					"limit; the turn ended with the objection standing", o.Bounces)
			notice.Degraded = []string{"the end-of-turn check was never satisfied"}
		}
		out = append(out, notice)
	}

	// The written-path list is what the check ran against, so a list that was
	// cut short is a check with a hole in it — whatever verdict it reached.
	if o.WroteTruncated {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("this turn changed more than %d files, and only the first %d were "+
				"tracked; anything checked here covers that subset, not the whole change",
				agent.MaxTrackedWrites, agent.MaxTrackedWrites),
			Degraded: []string{"the changed-file list was truncated"},
		})
	}

	// First among the response-shaped notices, because it invalidates the
	// result more completely than anything below it: there is no answer to
	// qualify.
	if o.FinalEmpty {
		out = append(out, outcomeNotice{
			Text: "the turn ended without an answer — the last response carried no text and no tool call, " +
				"so nothing was completed; re-run it, and if it repeats the model is looping rather than working",
			Degraded: []string{"turn ended with no answer"},
		})
	}
	if o.FinalTruncated {
		// Deliberately does not tell the operator to raise the cap.
		//
		// Hitting it has two opposite causes and this function cannot tell
		// them apart. An answer that was genuinely too long wants a bigger
		// budget. A model looping wants the opposite: the cap is the only
		// thing bounding it, and raising it buys a longer runaway. Measured on
		// 2026-08-19 by following this notice's own earlier advice — the cap
		// went 16,384 to 32,768, the looping step went from 560s to 979s and
		// 27,585 tokens, and the turn died on its wall-clock timeout instead.
		//
		// So it states the fact and names both readings. Deciding between them
		// needs a look at the answer, which the operator can do and this cannot.
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("the answer stopped at the %s output cap — it is cut off, not complete. "+
				"If the answer was going somewhere, raise the cap; if it was repeating itself, the cap is "+
				"what stopped it and raising it will only buy a longer loop",
				flags.LLMLocalMaxOutputTokens),
			Degraded: []string{"answer truncated by the output cap"},
		})
	}
	if o.TruncatedBySteps {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("the step ceiling ended this turn after %d steps — the work is not complete",
				o.Steps),
			Degraded: []string{"turn truncated by the step ceiling"},
		})
	}
	// Reported even on a turn that recovered: it is the explanation for a
	// context that grew faster than the work did.
	if o.TruncatedByTokens && !o.FinalTruncated {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("a response hit the %s output cap during this turn and the turn recovered",
				flags.LLMLocalMaxOutputTokens),
		})
	}
	// History no longer fits and compaction has nothing left to give. Ahead of
	// the compensations below because it predicts the next turn as much as it
	// describes this one.
	if o.ContextOverflowed {
		out = append(out, outcomeNotice{
			Text: "history still exceeded the context budget after everything compactable was compacted — " +
				"the server was asked to take more than it can hold, and what it dropped is its choice, not this " +
				"harness's; start a fresh session or raise the window",
			Degraded: []string{"history exceeded the context budget"},
		})
	}
	// A refusal and an unmappable stop are both terminal states that look
	// exactly like a completed turn from every other field: text arrived, no
	// tool calls, nothing truncated, nothing empty.
	switch o.StopReason {
	case llm.StopRefusal:
		out = append(out, outcomeNotice{
			Text:     "the model refused this request — the turn ended on a refusal, not on the work being done",
			Degraded: []string{"turn ended in a refusal"},
		})
	case llm.StopOther:
		out = append(out, outcomeNotice{
			Text: "the stream ended without a stop reason this adapter recognises: either the server sent one " +
				"that is not in its mapping, or the connection ended mid-response. The answer above may be " +
				"incomplete and nothing else will say so",
			Degraded: []string{"unrecognised stop reason"},
		})
	}
	// Ahead of the model-facing counts: this one is a defect in the harness.
	if o.Panicked > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("%d tool call(s) were refused because a stage of the tool pipeline panicked — "+
				"that is a defect in this harness, not in the model or the server; the stack was written to stderr",
				o.Panicked),
			Degraded: []string{"tool pipeline panicked"},
		})
	}
	if o.Malformed > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("%d tool call(s) could not be read as sent and were asked for again",
				o.Malformed),
			Degraded: []string{"tool calls arrived malformed"},
		})
	}
	// A server that had to be compensated for is misconfigured for the model it
	// serves, and the compensation is invisible in the answer it produced.
	if !o.Decoding.Clean() {
		if o.Decoding.FallbackFormat != "" {
			out = append(out, outcomeNotice{
				Text: fmt.Sprintf("tool calls were recovered from %s text: this server does not parse "+
					"tool calls for the model it is serving", o.Decoding.FallbackFormat),
				Degraded: []string{"tool calls recovered from text"},
			})
		}
		if o.Decoding.PrefillDisproved {
			out = append(out, outcomeNotice{
				Text: fmt.Sprintf("%s is set, but this server sends reasoning on its own channel — the "+
					"declaration was dropped for this turn. Left set it files every answer as reasoning "+
					"and the turn returns nothing; turn it off for this server",
					flags.LLMLocalAssumePrefill),
				Degraded: []string{"declared reasoning prefill contradicted by the server"},
			})
		}
		if o.Decoding.ReasoningReclassified {
			out = append(out, outcomeNotice{
				Text:     "a prefilled thinking tag was corrected: this server is not configured for the model it is serving",
				Degraded: []string{"reasoning tag corrected"},
			})
		}
	}
	if o.Denied > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("%d of %d tool call(s) were refused by the gate", o.Denied, o.ToolCalls),
		})
	}
	if o.Qualified > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("%d of %d tool call(s) were allowed but not on the rules alone",
				o.Qualified, o.ToolCalls),
		})
	}
	if o.Repeated > 0 || o.Stalled > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("%d tool call(s) were refused as verbatim repeats and %d for making no "+
				"progress; this turn spent %d of its %d-unit step budget",
				o.Repeated, o.Stalled, o.BudgetSpent, maxSteps),
		})
	}
	// An escalation is a bill the operator did not directly authorise for this
	// turn — they set a ceiling, not a tier — so the turn that spent it has to
	// say so. Without this line a turn that quietly ran at a higher tier is
	// indistinguishable, from the outside, from one that was simply expensive.
	if o.EffortRaised > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("reasoning effort was raised from %q to %q during this turn (%d time(s)) "+
				"because calls were refused for going in circles; %s is the ceiling that permitted it",
				o.EffortFrom, o.EffortTo, o.EffortRaised, flags.LLMEffortCeiling),
		})
	}
	if o.TruncatedBySteps && o.NoProgressSteps > 0 {
		out = append(out, outcomeNotice{
			Text: fmt.Sprintf("%d of those steps ran tool calls that changed nothing, which is why "+
				"%d steps spent the whole %d-unit budget",
				o.NoProgressSteps, o.Steps, maxSteps),
		})
	}
	return out
}
