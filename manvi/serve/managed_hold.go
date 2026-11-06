package serve

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/codingagent"
)

// What a managed run leaves in a checkout it could not give back, and how the
// runner remembers it.
//
// blockedCheckout is a checkout whose harness block was left in place, and why.
// That the block may be there is the record on disk, which this does not replace:
// it is process memory kept so a verifier that is refused is told why, and, for a
// hold, so the checkout stays held. Two kinds:
//
//   - a restore that failed (session nil): the reason is the restore's error.
//     Nothing is held; the next attempt on the checkout tries the restore again.
//   - a hold (session set): the run ended without its agent process confirmed
//     gone (Close did not report it reaped), so the block was not taken out from
//     under a process that may still be reading and writing the file, and no other
//     run is admitted on the checkout. A hold ends when the provider confirms the
//     exit, which a background reaper keeps asking for (see reap) and which then
//     takes the block out, or when Manvi restarts: the hold is process memory, and
//     the record on disk is then applied as any dead run's is.
type blockedCheckout struct {
	info   os.FileInfo
	reason string

	// Set on a hold only. job is the held run, and revisable says its stored
	// outcome may be revised when the hold ends (see reviseHeldRun).
	session   ManagedSession
	job       *managedJob
	revisable bool
	runID     string
	since     time.Time
	root      string
	file      string
	nonce     string
	// interval and attempts are the reaper's pause and bound, taken when the hold
	// is, so the reaper reads no package variable; gaveUp is set when it has used
	// its attempts without a confirmation.
	interval time.Duration
	attempts int
	gaveUp   bool
	// lastClose is what the provider's latest unconfirmed Close said (for a live
	// process group, which group), so a refusal can name what holds the checkout.
	lastClose string
	// rechecks counts the times an attempt on the checkout restarted a reaper that
	// had used its attempts.
	rechecks int
}

// reapInterval is the pause between the reaper's asks and maxReapAttempts how
// many it makes. Close itself waits up to five seconds for a process each time,
// so the defaults keep asking for about ten minutes. Tests shorten them.
var (
	reapInterval    = 5 * time.Second
	maxReapAttempts = 60
)

// holdMessage is what a refused attempt, and a refused verification, is told about
// a hold. It names the run, since when, and how it ends.
func (b *blockedCheckout) holdMessage() string {
	waiting := "Manvi keeps asking the provider and releases the checkout, taking the block out, when it confirms the exit"
	switch {
	case b.gaveUp:
		waiting = fmt.Sprintf("Manvi asked the provider %d times and has stopped asking; the next attempt on this checkout starts asking again", b.attempts)
	case b.rechecks > 0:
		waiting = fmt.Sprintf("Manvi had stopped asking after %d attempts and is re-checking with the provider now; it releases the checkout, taking the block out, when it confirms the exit", b.attempts)
	}
	if b.lastClose != "" {
		waiting = fmt.Sprintf("The provider last answered: %s. %s", b.lastClose, waiting)
	}
	return fmt.Sprintf("managed run %s ended at %s without confirmation that its agent process exited, so the agent may still be running: its harness block (nonce=%s) stays in %s and this checkout is held. %s. To recover without waiting, or if it never confirms, restart `manvi serve`: the record on disk is then applied as a dead run's block is",
		b.runID, b.since.UTC().Format(time.RFC3339), b.nonce, b.file, waiting)
}

// setBlocked remembers why the block on a checkout could not be taken out,
// replacing what was remembered for that directory.
func (r *ManagedRunner) setBlocked(info os.FileInfo, reason string) {
	if info == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropBlocked(info)
	r.blocked = append(r.blocked, &blockedCheckout{info: info, reason: reason})
}

func (r *ManagedRunner) clearBlocked(info os.FileInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropBlocked(info)
}

// dropBlocked forgets a checkout; mu is held.
func (r *ManagedRunner) dropBlocked(info os.FileInfo) {
	r.blocked = slices.DeleteFunc(r.blocked, func(b *blockedCheckout) bool { return sameCheckout(b.info, info) })
}

// blockedReason is why the block on a checkout was not taken out, or "".
func (r *ManagedRunner) blockedReason(info os.FileInfo) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.blocked {
		if b.session == nil && sameCheckout(b.info, info) {
			return b.reason
		}
	}
	return ""
}

// holdCheckout keeps a checkout held after a run whose agent process is not
// confirmed gone: its block stays in the file and its record on disk, and no run
// is admitted on the checkout until the reaper confirms the exit. It starts that
// reaper, one per held run.
func (r *ManagedRunner) holdCheckout(job *managedJob, session ManagedSession, m *harnessMaterialisation) {
	job.mu.Lock()
	info, root, revisable := job.checkoutInfo, job.checkout, job.revisableHold
	job.mu.Unlock()
	if info == nil {
		return
	}
	b := &blockedCheckout{info: info, session: session, job: job, revisable: revisable, runID: job.id, since: time.Now(), root: root, file: m.File, nonce: m.pending.Nonce, interval: reapInterval, attempts: maxReapAttempts}
	r.mu.Lock()
	r.dropBlocked(info)
	r.blocked = append(r.blocked, b)
	if r.reapStop == nil {
		r.reapStop = make(chan struct{})
	}
	if r.closed {
		r.stopOnce.Do(func() { close(r.reapStop) }) // a hold taken while closing is not asked about
	}
	stop := r.reapStop
	r.mu.Unlock()
	go r.reap(b, stop)
}

// heldReason is why a checkout is held for an unconfirmed exit, or "".
func (r *ManagedRunner) heldReason(info os.FileInfo) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, b := range r.blocked {
		if b.session != nil && sameCheckout(b.info, info) {
			return b.holdMessage()
		}
	}
	return ""
}

// reap asks the provider of a held checkout, off the managed lane, whether its
// process has exited, until it says so, the runner closes, or the attempts are
// used. Close is idempotent and bounded, so one stuck process costs this goroutine
// its waits and nobody else anything: an attempt on the held checkout is refused
// at once, and every other checkout goes on.
func (r *ManagedRunner) reap(b *blockedCheckout, stop <-chan struct{}) {
	for attempt := 0; attempt < b.attempts; attempt++ {
		exit, err := b.session.Close()
		if exitConfirmed(exit) {
			r.releaseHeld(b, exit, err)
			return
		}
		if err != nil {
			r.mu.Lock()
			b.lastClose = err.Error()
			r.mu.Unlock()
		}
		select {
		case <-stop:
			return
		case <-time.After(b.interval):
		}
	}
	r.mu.Lock()
	b.gaveUp = true
	r.mu.Unlock()
}

// releaseHeld ends a hold whose process is confirmed gone: the held run's block is
// taken out of the file (restoreHeldBlock: only if the record is this run's), and
// only then does the checkout stop being held, so no attempt is admitted between.
// A restore that fails, or a block the agent edited inside, keeps the record and is
// remembered, as it is when a run ends normally: HarnessBusy refuses naming it, and
// the next attempt on the checkout reports it.
func (r *ManagedRunner) releaseHeld(b *blockedCheckout, exit codingagent.Exit, closeErr error) {
	if !r.releaseHold(b) {
		return
	}
	r.reviseHeldRun(b, exit, closeErr)
}

// releaseHold takes a hold's block out and ends the hold, reporting whether this
// call did: a hold already ended (by an earlier or a concurrent reap) is not ended
// twice, and its block, which may by now be another run's, is not touched again.
func (r *ManagedRunner) releaseHold(b *blockedCheckout) bool {
	r.checkoutMu.Lock()
	defer r.checkoutMu.Unlock()
	r.mu.Lock()
	held := slices.Contains(r.blocked, b)
	r.mu.Unlock()
	if !held {
		return false
	}
	// Restore first and only then stop holding, so no attempt is admitted between.
	err := restoreHeldBlock(b.root, b.nonce)
	r.mu.Lock()
	r.blocked = slices.DeleteFunc(r.blocked, func(e *blockedCheckout) bool { return e == b })
	r.mu.Unlock()
	if err != nil {
		err = fmt.Errorf("run %s, whose agent process has since exited: %w", b.runID, err)
	}
	r.settleBlocked(b.info, b.root, err)
	return true
}

// reviseHeldRun settles the stored run of a hold that has just ended. The run was
// recorded unresolved because its Close could not confirm the agent's process
// group gone; a later Close that does is what that Close would have been, so the
// run is recorded as runOutcome says for it: exited with the agent's code, when
// this Close too has nothing to add, and left unresolved otherwise, as the first
// would have left it. It goes through recordRun, the writer of the first record,
// and only from unresolved: a run already finalised (by a reconcile, or by a
// concurrent revision) is not rewritten. A run that was not ended by its own
// Close (revisable false) is unresolved by design and stays so.
func (r *ManagedRunner) reviseHeldRun(b *blockedCheckout, exit codingagent.Exit, closeErr error) {
	if !b.revisable || b.job == nil {
		return
	}
	outcome, code := runOutcome(exit, closeErr)
	if outcome != "exited" {
		return
	}
	cause := errors.New("the agent process and its process group have since been confirmed gone; the outcome was unresolved only until then, and task acceptance still requires review")
	written, err := r.recordRun(b.job, outcome, code, cause, "", "unresolved")
	b.job.mu.Lock()
	defer b.job.mu.Unlock()
	switch {
	case err != nil:
		b.job.err = errors.Join(b.job.err, fmt.Errorf("revising run %s after its agent was confirmed gone: %w", b.runID, err))
	case written:
		b.job.phase = outcome
	}
}

// recheckHeld restarts the reaper of a hold whose attempts ran out, when an attempt
// on the checkout comes in: the agent may have exited since. It starts the same
// bounded reaper off the lane and returns at once; the attempt is refused either way.
func (r *ManagedRunner) recheckHeld(info os.FileInfo) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.reapStop == nil {
		return
	}
	for _, b := range r.blocked {
		if b.session != nil && b.gaveUp && sameCheckout(b.info, info) {
			b.gaveUp = false
			b.rechecks++
			go r.reap(b, r.reapStop)
			return
		}
	}
}

// exitConfirmed says whether the agent is gone: Close reaped it and found no member
// of its process group alive. It is what a harness run's block restore and the
// release of its checkout wait on, and it fails closed: a group member still alive
// after Close's SIGKILL and bounded wait (Exit.GroupLive) is an anomaly (a process
// in uninterruptible sleep, one this host may not signal, or a zombie that a host
// without a reaping init never collects) and is treated as the agent still running.
//
// A Close error beside a reaped process with the group gone (os/exec's
// ErrWaitDelay, when an orphaned grandchild still holds the agent's stdout) is
// about that pipe, not the agent, and is recorded on the run as a note; it does not
// keep a checkout held. The old reason for not looking at the group at all, that a
// dev server left behind by a finished run would hold every checkout, no longer
// applies: Close SIGKILLs the group before it looks, so such a process is dead by
// then. A descendant that left the group with setsid is invisible to the probe and
// holds nothing itself; nothing here chases it. Its children that stayed in the
// group are seen, and a zombie among them whose parent is outside the group and
// never waits cannot be told from a running member: the checkout then stays held,
// with Close's answer (which group) in the refusal, until that parent exits or
// the host restarts. That is the fail-closed side of the same limit.
//
// Only a harness run holds (finish restores or holds only a materialised block).
// Whether a plain run, which has no block, should also wait for the group is the
// owner's to decide and is not decided here.
func exitConfirmed(exit codingagent.Exit) bool { return exit.Reaped && !exit.GroupLive }

// unconfirmedSession is the session to hold a checkout for after a Close that returned
// exit: the session itself when the exit is not confirmed, else nil.
func unconfirmedSession(session ManagedSession, exit codingagent.Exit) ManagedSession {
	if exitConfirmed(exit) {
		return nil
	}
	return session
}

// settleBlocked is what an attempt to take a block out came to: err is its
// error. A record still on disk means the block may still be in the file, and
// why is remembered; with none there is nothing left to say.
func (r *ManagedRunner) settleBlocked(info os.FileInfo, root string, err error) {
	if err == nil || !harnessRecordPending(root) {
		r.clearBlocked(info)
		return
	}
	r.setBlocked(info, err.Error())
}
