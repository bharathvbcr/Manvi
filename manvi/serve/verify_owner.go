package serve

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// A running verification record names the process that claimed it. Only that
// process may settle it as "the process ended": a second process reading the
// record cannot tell a job that died from one that is still running in the
// first, and settling a live owner's record appends a degraded row beneath its
// real verdict.

// verifyOwner is the process that claimed a verification. Start is that
// process's start time (codingagent.ProcessStart), which is what tells it from
// a later process handed the same pid; zero when the platform could not read it.
type verifyOwner struct {
	PID   int   `json:"pid"`
	Start int64 `json:"start,omitempty"`
}

// storedVerifyRecord is a record as the store holds it: the wire record, and
// beside it the owner. The owner is storage-only. The wire record is the
// contract a host reads (contracts/serve.ops.schema.json), and it carries no
// process identity; a stored row with one extra top-level key is read the same
// by every reader that decodes the fields it knows.
type storedVerifyRecord struct {
	VerifyRecord
	Owner *verifyOwner `json:"owner,omitempty"`
}

// processStartOf reads another process's start time. A variable so a test can
// stand in for the platform; production is codingagent.ProcessStart.
var processStartOf = codingagent.ProcessStart

// currentVerifyOwner is this process.
func currentVerifyOwner() verifyOwner {
	owner := verifyOwner{PID: os.Getpid()}
	if start, err := processStartOf(owner.PID); err == nil {
		owner.Start = start
	}
	return owner
}

// verifyOrphanGrace is added to a job's deadline before an unattributable claim
// is called abandoned: the settlement after a deadline has its own ten seconds,
// and the clocks of two processes on one host differ by less than the rest.
const verifyOrphanGrace = time.Minute

// ownerEnded says whether the process that claimed a running record has ended,
// and on what basis. It is asked only about a record this process holds no job
// for. The answer is "ended" in exactly these cases, each naming its basis:
//
//   - the owner is this process: it holds no job for its own claim, so nothing
//     is running it (a settlement that could not be written, or a restart that
//     reused neither pid nor start time).
//   - the owner's pid names no process (codingagent.ErrNoProcess).
//   - the owner's pid names a process with another start time: the pid was
//     handed on, so the claimant is gone. That includes this process's own pid
//     after a restart.
//   - process identity cannot be established (no owner on the record, or a
//     platform that cannot read start times) and the claim is older than any
//     job can run: deadline plus settlement. A live owner would have settled it.
//
// Anything else leaves the record running, including a liveness check that
// failed: an unreadable answer is not an ended process.
func (r *VerifyRunner) ownerEnded(owner *verifyOwner, startedAt string) (basis string, ended bool) {
	if owner != nil {
		if owner.PID == r.owner.PID && (owner.Start == 0 || r.owner.Start == 0 || owner.Start == r.owner.Start) {
			return "this process claimed it and holds no job for it", true
		}
		start, err := processStartOf(owner.PID)
		switch {
		case errors.Is(err, codingagent.ErrNoProcess):
			return fmt.Sprintf("process %d, which claimed it, no longer exists", owner.PID), true
		case err == nil && owner.Start != 0 && start != owner.Start:
			return fmt.Sprintf("process %d now has start time %d, not the %d it claimed this with, so the pid was handed on", owner.PID, start, owner.Start), true
		case err == nil:
			return fmt.Sprintf("process %d, which claimed it, is still running", owner.PID), false
		}
	}
	claimed, err := time.Parse(time.RFC3339, startedAt)
	if err != nil {
		return "the claim time is unreadable", false
	}
	if limit := r.deps.Timeout + verifyOrphanGrace; r.deps.Now().Sub(claimed) > limit {
		return fmt.Sprintf("the claim is older than the job deadline of %s plus settlement, and no process identity could be checked", r.deps.Timeout), true
	}
	return "no process identity could be checked and the claim is within the job deadline", false
}
