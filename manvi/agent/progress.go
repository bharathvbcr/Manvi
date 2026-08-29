package agent

import (
	"crypto/sha256"
	"slices"
	"strings"

	"manvi/tools"
)

// This file answers one question for the loop: did that step get anywhere?
//
// What counts as progress, stated plainly so it can be argued with:
//
//	A dispatched tool call is progress when it either
//	  (a) returned something this turn had not already been told — a result
//	      whose (tool name, error flag, text) triple is new — or
//	  (b) ran a tool that can mutate and did not report an error.
//
// Everything else is a step that changed nothing observable.
//
// (a) is the honest form of "new information". It deliberately does *not*
// include the arguments in the key, because that is exactly the hole the
// verbatim repeat ledger has: observed against the live model, a request to
// remove unused imports produced greps for `sys|time`, `time\.`, `time\b`,
// `time.sleep` and `sys\.` — five distinct argument strings, five identical
// empty results, one file left untouched and 91k input tokens spent. Keying on
// what came back rather than what was asked is what makes near-duplicates
// visible; keying on the arguments is what made them invisible.
//
// (b) exists because the loop cannot see inside a tool. It has no way to know
// whether a write actually changed bytes on disk or an exec had an effect, and
// inventing an answer would be fabricating progress. What it can observe is the
// registry's own ReadOnly flag — the same flag that decides which tools a
// read-only search agent is offered — so "a tool that is allowed to mutate ran
// and did not error" is taken as progress. That over-credits: a write of
// identical content counts. The asymmetry is deliberate. A missed stall costs
// at most the rest of the step budget, which is bounded; a stall declared over
// real work refuses something the model was right to do, and it cannot tell
// that refusal apart from the ones it should heed.
//
// A policy-decided result (Rule set) is never claimed as a mutation: the gate
// may have short-circuited the body, and the loop is not told whether it ran.
// Its text is still new information the first time, which is (a)'s job.
//
// The limits worth naming: two different empty files read in a row look like
// one file read twice, and a model that keeps learning genuinely new facts
// while never acting on them is not detectable here at all. Detecting that
// would mean judging whether the work serves the task, which is not a decision
// this loop can make without a model of the task.
//
// The mirror of that last one is the widest gap here, and it is deliberate. A
// model that keeps *mutating* without converging — rewriting one file forty
// times with forty different contents — is credited progress on every call and
// is never interrupted. Measured directly: forty such writes ran to completion
// with stalled=0 and repeated=0. Every one of them changed the world, so by
// rule (b) every one was progress, and the only honest alternative would be to
// judge whether the edits were getting closer to something, which is the
// judgement this loop has already declined to make.
//
// It is bounded rather than detected. No step costs less than one unit, so
// MaxSteps caps it absolutely, and `manvi run` puts a wall-clock timeout around
// the whole turn. That is a real ceiling, not a claim that the case is handled:
// a thrashing turn can still burn its budget. A "same path written N times in a
// row" rule would catch it, and was not added on purpose — a model that writes,
// tests, and writes again is doing exactly the right thing, and a limiter that
// refuses that is worse than one that misses the thrash, for the same reason
// the asymmetry above runs the way it does.

// NoProgressLimit is how many consecutive tool-running steps may produce no
// observable progress before the next call is refused rather than run.
//
// Three, because two is inside the range of ordinary work — a look that comes
// back empty followed by a second that comes back the same way is a plausible
// two steps of an honest search — and because the refusal costs a step of its
// own, so a limit that fires early on real work would itself be the churn it
// is meant to stop.
const NoProgressLimit = 3

// StallCost is what a step that produced nothing observable charges against the
// step budget, against 1 for a step that got somewhere.
//
// Three keeps the ceiling meaningful for a spinning turn — a turn that never
// makes progress ends after MaxSteps/3 steps rather than MaxSteps — while
// leaving a turn that recovers most of its budget intact. Because no step ever
// costs less than 1, MaxSteps remains a hard ceiling on the number of steps: it
// is a budget that can be spent faster, never one that can be extended.
const StallCost = 3

// progressTracker decides, per step, whether the turn got anywhere, and holds
// the streak of steps that did not. It is per-turn state: a turn that ended in
// circles says nothing about the next one.
type progressTracker struct {
	// mutating is the set of registered tools not marked ReadOnly, taken from
	// the registry at the start of the turn. A tool that is not registered at
	// all is not mutating — an unknown-tool error changed nothing.
	mutating map[string]bool
	// seen holds a digest per distinct result already returned this turn. It is
	// not capped: its size is bounded already by MaxSteps and by how many calls
	// one response can carry under the output cap, and it is released with the
	// turn. A cap would have to start crediting progress it had not checked,
	// which is a check reporting the same answer as a check that ran.
	seen map[[32]byte]struct{}
	// streak counts consecutive steps that ran tool calls and produced no
	// observable progress.
	streak int
}

func newProgressTracker(registry *tools.Registry) *progressTracker {
	p := &progressTracker{
		mutating: map[string]bool{},
		seen:     map[[32]byte]struct{}{},
	}
	if registry == nil {
		return p
	}
	for _, schema := range registry.Schemas() {
		p.mutating[schema.Name] = true
	}
	for _, schema := range registry.ReadOnlySchemas() {
		delete(p.mutating, schema.Name)
	}
	return p
}

// stalled reports whether the turn has spent NoProgressLimit consecutive
// tool-running steps producing nothing observable.
func (p *progressTracker) stalled() bool {
	return p != nil && p.streak >= NoProgressLimit
}

// interrupted clears the streak after a refusal has been handed to the model.
//
// The refusal is itself something the model has not been told before, so it
// gets a clean run at the task. Holding the streak at the limit instead would
// refuse every call for the rest of the turn, and a turn that can no longer
// call a tool is over without having said so.
func (p *progressTracker) interrupted() {
	if p != nil {
		p.streak = 0
	}
}

// trackWrites folds one call's changed paths into the turn's set.
//
// Ordered, de-duplicated and capped. Ordered because a checker's argument list
// and a session event both read better in the order the work happened;
// de-duplicated because a file edited eight times is one file to verify; capped
// because the set is carried into both, and an unbounded one is a turn that can
// make its own evidence unreadable.
//
// The cap reports itself. A truncated list handed on as though it were complete
// is how a check that examined 256 of 900 files comes to be recorded as having
// examined all of them, and "never present a capped sample as complete
// coverage" is the rule that failure breaks.
func trackWrites(have []string, truncated bool, add []string) ([]string, bool) {
	for _, p := range add {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if slices.Contains(have, p) {
			continue
		}
		if len(have) >= MaxTrackedWrites {
			return have, true
		}
		have = append(have, p)
	}
	return have, truncated
}

// observe records the outcome of one dispatched call and reports whether it was
// progress. Refused calls must not be passed here: they never ran, so they can
// neither demonstrate progress nor its absence.
func (p *progressTracker) observe(name string, result tools.Result) bool {
	if p == nil {
		return true
	}
	key := resultDigest(name, result)
	_, repeat := p.seen[key]
	p.seen[key] = struct{}{}
	if !repeat {
		return true
	}
	// The same answer as before. Only a tool that could have changed something
	// counts now.
	return p.mutating[name] && !result.IsError && result.Rule == ""
}

// mutated reports whether this call could have changed the world and did not
// error. It is the same test rule (b) above applies, named and exported to the
// loop because the terminal checkpoint asks the same question for a different
// reason: it wants to know whether the turn has anything to verify.
//
// It stays deliberately wide — a shell command that ran is a mutation here even
// though this cannot see what it did — and that width is why it is not the
// signal a verifier runs against. Wide is right for "is there anything to check
// at all", and wrong for "which files". tools.Result.Wrote answers the second,
// and the two must not be collapsed: a turn where the model ran `git log`
// through the shell sets this and names no path, and a checker handed that as a
// file list would either check nothing and report a pass, or refuse a turn that
// wrote nothing.
func (p *progressTracker) mutated(name string, result tools.Result) bool {
	if p == nil {
		return false
	}
	return p.mutating[name] && !result.IsError && result.Rule == ""
}

// endStep folds a finished step into the streak. It returns what the step costs
// against the budget, and whether it was a step that ran work and got nowhere.
func (p *progressTracker) endStep(calls, ran int, progressed bool) (cost int, noProgress bool) {
	if p == nil {
		return 1, false
	}
	switch {
	case calls == 0:
		// Text only. Whether prose advanced the task is not something this loop
		// can judge, and guessing in either direction would be inventing an
		// answer, so it charges the baseline and leaves the streak alone.
		return 1, false
	case ran == 0:
		// Every call was refused, by the repeat ledger or by this tracker.
		// Nothing ran, so nothing can have changed, and the step is charged as
		// the waste it was. The streak is left as the refusal left it.
		return StallCost, false
	case progressed:
		p.streak = 0
		return 1, false
	default:
		p.streak++
		return StallCost, true
	}
}

// resultDigest keys a result by what came back, never by what was asked for.
// The error flag is part of the key: a failure and a success carrying the same
// text are different outcomes.
func resultDigest(name string, result tools.Result) [32]byte {
	flag := "ok\x00"
	if result.IsError {
		flag = "err\x00"
	}
	return sha256.Sum256([]byte(name + "\x00" + flag + result.Text))
}
