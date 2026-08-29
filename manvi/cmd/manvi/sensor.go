package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"manvi/agent"
	"manvi/agents"
	"manvi/core/bus"
	"manvi/devcouncil"
	"manvi/flags"
	"manvi/session"
)

// The end-of-turn check, and the escalation ladder above it.
//
// What this replaces is a sentence in the system prompt. devcouncil_verify_task
// carries the advice "call this before claiming done", and a model that does
// not call it finishes anyway — the harness registered no terminal checkpoint
// listener at all, so a turn that wrote nine files and verified none of them
// ended looking exactly like one that verified all of them. Advice is not a
// gate. Nagging harder is not a gate either; the fix is that the harness runs
// the check itself and the producer cannot skip it.
//
// The ladder is deliberately the one agent/effort.go already argued for, rather
// than a second one with different reasoning. That file considered classifying
// the prompt up front to decide how hard to think, and rejected it: it would be
// a judgement made on the least information the harness will ever have, before
// a single token has been generated. It escalates on evidence instead — the
// repeat ledger and the progress tracker, both of which only exist *after* the
// turn has demonstrated something.
//
// The same rule decides when a second opinion is worth its price here. Every
// turn starts alone. A check that fails once is answered by telling the model
// what failed, which is cheap and is usually enough. Only a turn that has been
// told and has failed again — or one already going in circles by the loop's own
// measure — is worth a critic, and by then "this turn is stuck" is a fact
// rather than a guess about the prompt.

// pathVerifier is the check itself, named here by what this file needs rather
// than by the type that provides it.
//
// One production implementation — *devcouncil.Registry, which owns the gates
// and the repository root — and it is declared as an interface for a reason
// this package cares about: the sensor's own decisions (when to skip, when to
// bounce, when to escalate, what to record) are the part most likely to be
// wrong, and testing them through a real repository and a real Rust binary
// would test the wrong thing slowly.
type pathVerifier interface {
	// VerifyPaths runs the end-of-turn check over the paths a turn changed.
	VerifyPaths(ctx context.Context, paths []string, command string) devcouncil.PathReport
	// ExistingPaths splits a path list by what is still on disk, so a deleted
	// file is a stated exclusion rather than a silent one.
	ExistingPaths(paths []string) (present, missing []string)
}

// sensor is the harness's own end-of-turn check.
type sensor struct {
	native pathVerifier
	log    *session.Log
	flags  *flags.Registry
	// runner dispatches the critic. Nil when this harness has no sub-agent
	// runner attached, which is a reason to skip the escalation and say so —
	// never a reason to report a turn as checked.
	runner devcouncil.SubAgentRunner
	// command is the operator's own verification command. See
	// operatorVerifyCommand for why it may only come from operator scope.
	command string
	// criticDispatched bounds the escalation to one critic per turn. A second
	// critic on the same turn would be asked the same question about the same
	// tree, and would cost a full child turn to produce the answer already in
	// hand.
	criticDispatched bool
	// turn is the turn the counters above belong to, so they reset when the
	// turn does rather than when whoever owns the bus decides to make a new one.
	turn int
}

// verifyCommandEnv is the only place an operator verification command is read
// from.
//
// Deliberately not a flag. This harness's settings load from
// .devcouncil/config.yaml; the restricted-path rung that keeps an agent out of
// that file is inside the hard-rules block, and a relaxed posture switches that
// block off. A verification command sourced from the settings registry would
// therefore be a command the agent can write into the repository and the
// harness then executes with its own authority at the end of every mutating
// turn, outside the gate the agent's own commands pass. Reading it from the
// process environment puts it back where the operator is: a value the agent
// cannot reach from inside the repository.
const verifyCommandEnv = "MANVI_VERIFY_COMMAND"

// operatorVerifyCommand returns the operator's verification command, or empty.
func operatorVerifyCommand() string {
	return strings.TrimSpace(os.Getenv(verifyCommandEnv))
}

// fetchHostsEnv is the only place the documentation-lookup allowlist is read
// from, for exactly the reason above: a settings file inside the repository is
// a file the agent can write under a relaxed posture, and an allowlist the
// agent can extend is not an allowlist.
const fetchHostsEnv = "MANVI_FETCH_HOSTS"

// operatorFetchHosts returns the hosts this harness may reach, comma or
// space separated. Empty means no network access, which is the default.
func operatorFetchHosts() []string {
	raw := os.Getenv(fetchHostsEnv)
	var hosts []string
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	}) {
		if host := strings.TrimSpace(field); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}

// attachSensor registers the check on a loop's own bus.
//
// It returns whether anything was registered, so a caller can tell "no check
// was configured" from "the check passed" — the two look identical from the
// outcome alone, and only one of them is good news.
func attachSensor(b *bus.Bus, s *sensor) error {
	if b == nil || s == nil {
		return nil
	}
	_, err := bus.OnSerial(b, s.check)
	return err
}

// check is the terminal checkpoint listener.
//
// It never returns an error for a finding. An error here means the listener
// itself broke, and the loop's answer to that is to close the turn as degraded
// — which is right for a broken listener and wrong for a failed verification,
// because a failed verification is exactly the case that wants another step.
func (s *sensor) check(ctx context.Context, e *agent.TurnStopping) error {
	if e.Turn != s.turn {
		s.turn = e.Turn
		s.criticDispatched = false
	}

	// A cut-off answer is not a finished one. Handled before the mutation test
	// because it applies to a question as much as to an edit: a turn that ran
	// out of room mid-sentence has not answered, whatever else it did.
	if e.Truncated && e.Bounce == 0 {
		e.Verdict = agent.SensorFailed
		e.Inject = "Your previous answer stopped at the output limit, so it is cut off rather " +
			"than complete. Finish it — briefly. Do not restate what you already said."
		return s.record(e, session.VerifyReportData{
			Verdict: string(agent.SensorFailed), Source: "output cap",
			Findings: []string{"the answer was truncated by the output cap"},
			Bounce:   e.Bounce,
		})
	}

	if !e.Mutated {
		// Nothing was changed, so there is nothing to verify. Recorded rather
		// than passed over in silence: "the check was not owed" and "the check
		// did not happen" are different facts about a turn, and only the log
		// can carry the difference to a reader who was not there.
		e.Verdict = agent.SensorSkipped
		return s.record(e, session.VerifyReportData{
			Verdict: string(agent.SensorSkipped), Source: "no change",
			Bounce: e.Bounce,
		})
	}

	report := s.run(ctx, e)
	verdict := verdictOf(report)
	e.Verdict = verdict

	data := session.VerifyReportData{
		Verdict: string(verdict), Source: report.Source,
		Paths: report.Examined, Findings: report.Findings,
		Degraded: report.Degraded, Bounce: e.Bounce,
	}
	if e.WroteTruncated {
		data.Degraded = append(data.Degraded,
			"this turn changed more paths than it tracked, so the examined set is incomplete")
	}
	if err := s.record(e, data); err != nil {
		return err
	}

	if verdict == agent.SensorPassed {
		return nil
	}
	// A degraded check does not bounce. The model cannot fix a missing verifier
	// binary, and sending it back to try would spend a model call teaching it
	// that. The degradation is on the outcome and in the log, where the person
	// who can fix it will see it.
	if verdict == agent.SensorDegraded {
		return nil
	}

	e.Inject = s.injection(ctx, e, report)
	return nil
}

// run performs the check itself.
func (s *sensor) run(ctx context.Context, e *agent.TurnStopping) devcouncil.PathReport {
	if s.native == nil {
		return devcouncil.PathReport{
			Verdict: devcouncil.VerdictDegraded, Source: "unconfigured",
			Degraded: []string{"no verifier is attached to this harness, so nothing was checked"},
		}
	}

	// Deleted paths are separated before the check rather than inside it. A
	// file that is gone cannot be read by a content gate, and handing it to one
	// produces either an error that reads as a finding about the code or a
	// silent skip — and a silent skip is the failure mode this whole file
	// exists to remove.
	present, missing := s.native.ExistingPaths(e.Wrote)
	report := s.native.VerifyPaths(ctx, present, s.command)
	for _, m := range missing {
		report.Skipped = append(report.Skipped, m+": no longer on disk")
	}

	// A turn that mutated and named no path at all is the shell case: the model
	// changed something through a command, and no handler could say what. The
	// operator's own command still covers it; nothing else does, and that gap
	// is stated rather than passed over.
	if len(e.Wrote) == 0 && s.command == "" {
		report.Verdict = devcouncil.VerdictDegraded
		report.Degraded = append(report.Degraded,
			"this turn changed something through a command, so no file list was available; "+
				"set "+verifyCommandEnv+" to have the project's own check run here")
	}
	return report
}

// injection composes what the model is told, and decides whether a critic is
// worth dispatching for it.
func (s *sensor) injection(ctx context.Context, e *agent.TurnStopping, report devcouncil.PathReport) string {
	var b strings.Builder
	b.WriteString("The end-of-turn check ran against the files this turn changed and did not pass. " +
		"This is the harness reporting, not the operator.\n\n")
	for _, f := range report.Findings {
		b.WriteString("  - ")
		b.WriteString(f)
		b.WriteString("\n")
	}

	// The escalation, on evidence. A first failure is answered with the
	// findings alone, because a model that has just been told what is wrong
	// usually fixes it and a critic would be paying for a second opinion nobody
	// needed. A second failure — or a turn the loop already judges to be going
	// in circles — has demonstrated that telling it is not working.
	if e.Bounce >= 1 || e.Circling >= agent.NoProgressLimit {
		if verdict, note := s.dispatchCritic(ctx, e, report); note != "" {
			b.WriteString("\n")
			b.WriteString(note)
			b.WriteString("\n")
			if verdict.Judged && len(verdict.Findings) > 0 {
				for _, f := range verdict.Findings {
					b.WriteString("  - ")
					b.WriteString(f)
					b.WriteString("\n")
				}
			}
		}
	}

	b.WriteString("\nFix what is listed above, or say plainly that you cannot and why. " +
		"Do not repeat the previous answer.")
	return b.String()
}

// dispatchCritic runs one adversarial reviewer over the turn's changes.
//
// The child runs on its own bus and its own log, so nothing it does re-enters
// this checkpoint: there is no recursion to bound here beyond the one-per-turn
// rule above. It is bounded anyway by the sub-agent step ceiling, which is far
// below the parent's.
func (s *sensor) dispatchCritic(ctx context.Context, e *agent.TurnStopping, report devcouncil.PathReport) (devcouncil.SubAgentVerdict, string) {
	if s.criticDispatched {
		return devcouncil.SubAgentVerdict{}, ""
	}
	if s.runner == nil {
		return devcouncil.SubAgentVerdict{}, ""
	}
	// The delegation bound applies to the harness's own child as much as to the
	// model's. An operator who set the depth to zero asked for a harness that
	// delegates nothing, and a critic dispatched anyway would be this code
	// deciding its own errand is exempt.
	if bounds := agents.ResolveBounds(s.flags); bounds.MaxDepth < 1 {
		return devcouncil.SubAgentVerdict{}, ""
	}
	s.criticDispatched = true

	var b strings.Builder
	b.WriteString("An automated check has failed twice on the same change and the producing agent " +
		"has not resolved it. Review these files and say what is actually wrong.\n\n")
	// The claim, not only the files. A reviewer handed a diff can say whether
	// the code is sound; a reviewer handed the diff *and* what the producer
	// asserted about it can say whether the assertion is true, which is the
	// question that was actually failed. Falsifying a stated claim is a
	// sharper task than open-ended review, and it is the one that catches a
	// change that is locally correct and does not do what it says.
	if claim := strings.TrimSpace(e.Response.Message.Text()); claim != "" {
		b.WriteString("What the producing agent says it did:\n")
		b.WriteString(indentLines(claim))
		b.WriteString("\n\nTreat that as a claim to falsify, not as background.\n\n")
	}
	for _, p := range report.Examined {
		b.WriteString("  ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	if len(report.Findings) > 0 {
		b.WriteString("\nWhat the check reported:\n")
		for _, f := range report.Findings {
			b.WriteString("  - ")
			b.WriteString(f)
			b.WriteString("\n")
		}
	}

	result, err := s.runner.RunSubAgent(ctx, devcouncil.SubAgentRequest{
		Label:   "checkpoint-critic",
		Prompt:  b.String(),
		Verdict: verdictMarker,
	})
	if err != nil {
		// Named, not swallowed. A critic that could not run is a check that did
		// not happen, and the model is told that rather than being left to read
		// the silence as approval.
		return devcouncil.SubAgentVerdict{}, fmt.Sprintf(
			"A reviewer was dispatched and could not complete (%v), so no second opinion is "+
				"available here.", err)
	}
	verdict := result.Verdict.Reconcile()
	switch {
	case !verdict.Judged:
		return verdict, "A reviewer was dispatched and did not reach a verdict, which is not " +
			"approval. Its notes:\n" + indentLines(result.Summary)
	case verdict.Passed:
		return verdict, "A reviewer looked at the same change and found nothing blocking. The " +
			"check above still failed, so reconcile the two before finishing."
	default:
		return verdict, "A reviewer looked at the same change and agrees it is not ready:"
	}
}

// record writes the check's own account into the session log.
func (s *sensor) record(e *agent.TurnStopping, data session.VerifyReportData) error {
	if s.log == nil {
		return nil
	}
	_, err := s.log.Append(session.VerifyReport, data)
	return err
}

// verdictOf maps the verifier's own words onto the loop's.
//
// Written as an exhaustive switch with an explicit default rather than a map,
// so a verdict this does not recognise becomes degraded — a value nobody
// anticipated is not evidence that anything passed.
func verdictOf(report devcouncil.PathReport) agent.SensorVerdict {
	switch report.Verdict {
	case devcouncil.VerdictPassed:
		return agent.SensorPassed
	case devcouncil.VerdictFailed:
		return agent.SensorFailed
	case devcouncil.VerdictDegraded:
		return agent.SensorDegraded
	default:
		return agent.SensorDegraded
	}
}

// indentLines indents a block for inclusion in an injected message, and bounds
// it: a child's whole answer pasted into the parent's context is a child
// deciding how much of the parent's window to spend.
func indentLines(s string) string {
	s = truncateRunes(strings.TrimSpace(s), maxCriticSummaryRunes)
	var b strings.Builder
	for line := range strings.Lines(s) {
		b.WriteString("  ")
		b.WriteString(line)
	}
	return b.String()
}

// maxCriticSummaryRunes bounds a critic's prose where it enters the parent's
// history.
const maxCriticSummaryRunes = 2000
