package serve

import (
	"encoding/json"
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

// processZombieOf says whether a process has exited and not been reaped; a
// variable for the same reason.
var processZombieOf = codingagent.ProcessZombie

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

// decodeStoredVerify reads a stored row: the wire record, and the owner beside
// it. An owner that is missing, does not parse or names an impossible pid is an
// unknown owner (nil), never a reason to drop the row: the record is the verdict
// and the owner is only evidence about who may settle it.
func decodeStoredVerify(data []byte) (VerifyRecord, *verifyOwner, error) {
	var record VerifyRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return VerifyRecord{}, nil, err
	}
	return record, storedOwner(data), nil
}

// storedOwner is the owner key of a stored row, or nil when there is none or it
// cannot be trusted as a process identity.
func storedOwner(data []byte) *verifyOwner {
	var sidecar struct {
		Owner json.RawMessage `json:"owner"`
	}
	if json.Unmarshal(data, &sidecar) == nil && len(sidecar.Owner) > 0 {
		var owner verifyOwner
		if json.Unmarshal(sidecar.Owner, &owner) == nil && owner.PID > 0 && owner.Start >= 0 {
			return &owner
		}
	}
	return nil
}

// ownerEnded says whether a running record this process holds no job for may be
// settled as "the process that was verifying it ended", and on what basis. It is
// "ended" in exactly these cases, each naming its basis in the degraded line:
//
//   - the owner is this process: it holds no job for its own claim, so nothing
//     is running it (a settlement that could not be written, or a restart that
//     reused neither pid nor start time).
//   - the owner's pid names no process (codingagent.ErrNoProcess).
//   - the owner's pid names a process with another start time: the pid was
//     handed on, so the claimant is gone. That includes this process's own pid
//     after a restart.
//   - the owner is a zombie: it exited and was not reaped, so the kernel still
//     lists it with its start time, and it runs nothing.
//   - the claim is older than the job deadline plus a minute of settlement,
//     whatever else is known about the owner. No live owner works past its own
//     deadline, so a claim that old is a wedged or forged owner, or one whose
//     identity cannot be checked (no owner on the record, an unreadable owner, a
//     platform that cannot read start times); the basis says which.
//
// A record younger than that whose owner is alive, or whose liveness could not be
// read, stays running: an unreadable answer is not an ended process, and the age
// ceiling is what bounds it.
func (r *VerifyRunner) ownerEnded(owner *verifyOwner, startedAt string) (basis string, ended bool) {
	claimed, parseErr := time.Parse(time.RFC3339, startedAt)
	limit := r.deps.Timeout + verifyOrphanGrace
	overAge := parseErr == nil && r.deps.Now().Sub(claimed) > limit
	if owner == nil {
		if overAge {
			return fmt.Sprintf("the claim is older than the job deadline of %s plus settlement, and it names no owner to check", r.deps.Timeout), true
		}
		return "the claim names no owner and is within the job deadline", false
	}
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
		if zombie, zerr := processZombieOf(owner.PID); zerr == nil && zombie {
			return fmt.Sprintf("the owner (process %d) is a zombie: it exited and was not reaped, so nothing is running this", owner.PID), true
		}
		if overAge {
			return fmt.Sprintf("the claim is older than the job deadline of %s plus settlement although the owner (process %d) looks alive", r.deps.Timeout, owner.PID), true
		}
		return fmt.Sprintf("process %d, which claimed it, is still running", owner.PID), false
	}
	if overAge {
		return fmt.Sprintf("the claim is older than the job deadline of %s plus settlement, and the owner's liveness could not be checked (%v)", r.deps.Timeout, err), true
	}
	return "the owner's liveness could not be checked and the claim is within the job deadline", false
}
