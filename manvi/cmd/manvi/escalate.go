package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/agent"
	"github.com/bharathvbcr/Manvi/manvi/agents"
	"github.com/bharathvbcr/Manvi/manvi/devcouncil"
	"github.com/bharathvbcr/Manvi/manvi/tools"
)

// Escalation on evidence.
//
// A child that cannot finish is moved up one tier, once, instead of the harness
// guessing from a prompt that a task is hard. The ladder per child is: the loop
// raises effort on its own model up to the session's ceiling (agent.EffortPlan);
// if the turn still comes back with evidence, the child is re-run on the next
// tier up; if that re-run shows evidence too, the child fails with a typed
// devcouncil.ExhaustedError. The advisor is the top of the ladder and never
// escalates.
//
// The re-run is a fresh conversation. Thinking blocks are bound to the model
// that produced them, and editing a history breaks the preserved thinking the
// 5.x models rely on, so the old transcript is not replayed: the new child gets
// the original prompt and a short account of what the first attempt left.

// maxEscalationCarryRunes bounds how much of the first attempt's answer travels
// into the re-run's prompt. The re-run needs to know what was tried, not to be
// handed a second full context window of it.
const maxEscalationCarryRunes = 1200

// escalationEvidence names what a finished attempt showed about being out of
// its depth, or returns "" when it showed nothing. Each is a fact about the
// run — never a reading of how hard its prompt looked, which is the up-front
// guess this design exists to avoid.
func escalationEvidence(o agent.Outcome, summary string, maxSteps int) string {
	switch {
	case o.TruncatedBySteps:
		return fmt.Sprintf("the step ceiling (%d) was reached before the turn finished", maxSteps)
	case summary == "":
		return fmt.Sprintf("an empty summary after %d step(s)", o.Steps)
	case o.Sensor == agent.SensorFailed && o.BouncesExhausted:
		return fmt.Sprintf("the end-of-turn check was still failing after %d attempt(s) to put it right", o.Bounces)
	}
	return ""
}

// nextTier is the tier one escalation moves a child to: the nearest tier above
// from whose model differs from from's. A tier on the same model is skipped —
// moving heavy to main when both are Opus would re-run the same model on a cold
// cache and call it an escalation. The advisor is the ceiling: nothing is above
// it, and a profile that does not hold from has no ladder to climb.
func nextTier(profile agents.TierProfile, from agents.Tier) (agents.Tier, bool) {
	current, ok := profile[from]
	if !ok || from.Rank() < 0 || strings.TrimSpace(current.Model) == "" {
		return "", false
	}
	for _, t := range agents.Tiers() {
		if t.Rank() <= from.Rank() {
			continue
		}
		spec, held := profile[t]
		if !held || strings.TrimSpace(spec.Model) == "" || spec.Model == current.Model {
			continue
		}
		return t, true
	}
	return "", false
}

// escalate re-runs a child that showed evidence, or says why it did not.
//
// first is the attempt that showed it and firstErr its error, which is non-nil
// only when the evidence was an empty answer. Every refusal to escalate leaves
// the first attempt's result and error exactly as they were, with the reason
// added to the summary: not escalating must never change what a child reports.
func (r *subAgentRunner) escalate(ctx context.Context, cfg *subAgentConfig, req devcouncil.SubAgentRequest,
	first attemptResult, firstErr error) (devcouncil.SubAgentResult, error) {
	declined := func(why string) (devcouncil.SubAgentResult, error) {
		if firstErr == nil && first.result.Summary != "" {
			first.result.Summary += "\n\n[this sub-agent: showed evidence of being stuck (" +
				first.evidence + ") and was not escalated: " + why + "]"
		}
		return first.result, firstErr
	}

	switch {
	case req.Tier == "":
		return declined("it was placed on the parent's model, not a tier")
	case !agents.ParsePlacement(req.ModelSpec, nil).Inherits():
		return declined("its role is pinned to a model by the operator")
	case req.Tier == agents.TierAdvisor:
		return declined("the advisor is the top tier")
	}
	to, ok := nextTier(cfg.tiers, req.Tier)
	if !ok {
		return declined("no tier above " + req.Tier.String() + " runs a different model")
	}
	if ctx.Err() != nil {
		return declined("the dispatch was cancelled")
	}
	if !req.Escalation.Take() {
		return declined(fmt.Sprintf("this fan-out's escalation cap (%d) is spent", req.Escalation.Cap()))
	}

	rerun := req
	rerun.Tier = to
	rerun.Prompt = escalatedPrompt(req.Prompt, req.Tier, first)
	second, err := r.attempt(ctx, cfg, rerun)

	record := devcouncil.Escalation{
		Child: req.Label, From: req.Tier, To: to, Evidence: first.evidence, ColdCache: true,
	}
	merged := mergeAttempts(first.result, second.result)
	merged.Escalations = []devcouncil.Escalation{record}
	if second.evidence != "" {
		return merged, &devcouncil.ExhaustedError{Child: req.Label, From: req.Tier, To: to, Evidence: second.evidence}
	}
	if err != nil {
		var typed *devcouncil.EscalationError
		if errors.As(err, &typed) {
			return merged, err
		}
		return merged, &devcouncil.EscalationError{
			Child: req.Label, From: req.Tier, To: to, Evidence: first.evidence, Err: err,
		}
	}
	merged.Summary += "\n\n[this sub-agent: escalated from " + req.Tier.String() + " to " + to.String() +
		" after " + first.evidence + "]"
	return merged, nil
}

// escalatedPrompt is the re-run's whole instruction: the original prompt, and
// what the first attempt left behind. Never the transcript.
func escalatedPrompt(prompt string, from agents.Tier, first attemptResult) string {
	var b strings.Builder
	b.WriteString(strings.TrimRight(prompt, "\n"))
	fmt.Fprintf(&b, "\n\nAn earlier attempt at this on the %s tier did not finish: %s.", from, first.evidence)
	if len(first.result.Wrote) > 0 {
		fmt.Fprintf(&b, " It had already changed: %s.", strings.Join(first.result.Wrote, ", "))
	}
	if note := strings.TrimSpace(first.result.Summary); note != "" {
		b.WriteString("\nWhat it reported, for what it is worth:\n")
		b.WriteString(devcouncil.TruncateRunes(note, maxEscalationCarryRunes))
	}
	b.WriteString("\nStart from the current state of the work and finish it.")
	return b.String()
}

// mergeAttempts is what a child that ran twice reports: both attempts' spend,
// both attempts' steps and writes, and the re-run's answer and judgement. The
// run meter already counted each attempt by the model that spent it; this is
// the per-child figure, so it must not drop the first.
func mergeAttempts(first, second devcouncil.SubAgentResult) devcouncil.SubAgentResult {
	out := second
	out.Steps += first.Steps
	out.Usage = second.Usage
	out.Usage.Add(first.Usage)
	out.Wrote, out.WroteTruncated = tools.MergeWrites(first.Wrote, first.WroteTruncated, second.Wrote)
	out.WroteTruncated = out.WroteTruncated || second.WroteTruncated
	return out
}
