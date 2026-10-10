package devcouncil

import (
	"fmt"
	"sync/atomic"

	"github.com/bharathvbcr/Manvi/manvi/agents"
)

// A child that shows evidence of being out of its depth is re-run once on the
// next tier up. This file is the part of that which the dispatcher owns: the
// shared cap on how many children of one fan-out may do it, the record of each
// escalation that goes in the report, and the two typed errors a ladder ends
// in. The ladder itself — which tier is next, what counts as evidence — needs
// the session's tier profile and lives with the runner.

// EscalationBudget is the number of re-runs one fan-out may spend.
//
// It is shared by every child of the dispatch and taken without a lock, because
// the children fail concurrently and a check-then-act on a plain counter would
// let eight failing children each see "one left". A cap exists at all because
// escalation moves work to a dearer model: left unbounded, a fan-out that is
// uniformly struggling — a bad prompt, a broken tool — would be re-run in full
// on the expensive tier, which is the failure it was meant to contain.
type EscalationBudget struct {
	capacity  int
	remaining atomic.Int64
}

// NewEscalationBudget sizes the cap for a dispatch of children: half of them,
// rounded up, so a lone child may still escalate and a wide fan-out never
// re-runs all of itself.
func NewEscalationBudget(children int) *EscalationBudget {
	if children < 1 {
		children = 0
	}
	b := &EscalationBudget{capacity: (children + 1) / 2}
	b.remaining.Store(int64(b.capacity))
	return b
}

// Cap is how many escalations the dispatch was allowed.
func (b *EscalationBudget) Cap() int {
	if b == nil {
		return 0
	}
	return b.capacity
}

// Take spends one escalation and reports whether there was one to spend. A nil
// budget has none: a request that carries no budget was not dispatched as part
// of a fan-out that sized one, and is never escalated on its own authority.
func (b *EscalationBudget) Take() bool {
	if b == nil {
		return false
	}
	for {
		left := b.remaining.Load()
		if left <= 0 {
			return false
		}
		if b.remaining.CompareAndSwap(left, left-1) {
			return true
		}
	}
}

// Escalation records one child's move up a tier, for the run report.
type Escalation struct {
	// Child is the child's label in the fan-out report.
	Child string `json:"child"`
	// From and To are the tiers the child ran on before and after.
	From agents.Tier `json:"from"`
	To   agents.Tier `json:"to"`
	// Evidence is what triggered it: a fact about the first attempt, never a
	// reading of its prompt.
	Evidence string `json:"evidence"`
	// ColdCache is true: the re-run is a fresh conversation on another model,
	// and a prompt cache is per model, so its first request writes the whole
	// prompt to the cache again. It is stated rather than left to be inferred
	// from the usage line, where the extra cache writes would otherwise look
	// like a child that was simply expensive.
	ColdCache bool `json:"cold_cache"`
}

// ExhaustedError ends a child's ladder: it was re-run on the next tier up and
// the re-run showed the same kind of evidence. There is nowhere further the
// harness will take it, so the child fails rather than reports a completion it
// did not earn.
type ExhaustedError struct {
	Child string
	// From and To are the tiers of the first attempt and of the re-run.
	From, To agents.Tier
	// Evidence is what the re-run showed.
	Evidence string
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("sub-agent %q is exhausted: it showed evidence of being stuck on %s (%s), "+
		"was re-run once on %s, and still did not finish", e.Child, e.From, e.Evidence, e.To)
}

// EscalationError is a re-run that could not happen or could not complete: the
// provider refused it, the spend cap could not cover it, the parent was
// cancelled. The ladder stops there; a failed escalation is never retried or
// climbed further, because the reason it failed is not a property of the tier.
type EscalationError struct {
	Child    string
	From, To agents.Tier
	Evidence string
	Err      error
}

func (e *EscalationError) Error() string {
	return fmt.Sprintf("sub-agent %q could not be re-run on %s after %s (%s): %v",
		e.Child, e.To, e.From, e.Evidence, e.Err)
}

func (e *EscalationError) Unwrap() error { return e.Err }
