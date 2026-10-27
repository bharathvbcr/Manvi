package serve

import (
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

	// Set on a hold only.
	session ManagedSession
	runID   string
	since   time.Time
	root    string
	file    string
	nonce   string
	// interval and attempts are the reaper's pause and bound, taken when the hold
	// is, so the reaper reads no package variable; gaveUp is set when it has used
	// its attempts without a confirmation.
	interval time.Duration
	attempts int
	gaveUp   bool
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
	if b.gaveUp {
		waiting = fmt.Sprintf("Manvi asked the provider %d times and has stopped asking", b.attempts)
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
	info, root := job.checkoutInfo, job.checkout
	job.mu.Unlock()
	if info == nil {
		return
	}
	b := &blockedCheckout{info: info, session: session, runID: job.id, since: time.Now(), root: root, file: m.File, nonce: m.pending.Nonce, interval: reapInterval, attempts: maxReapAttempts}
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
		if exit, _ := b.session.Close(); exitConfirmed(exit) {
			r.releaseHeld(b)
			return
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

// releaseHeld ends a hold whose process is confirmed gone: the block is taken out
// of the file as a dead run's is (the record is on disk), and only then does the
// checkout stop being held, so no attempt is admitted between. A restore that
// fails leaves the record and is remembered as any failed restore is.
func (r *ManagedRunner) releaseHeld(b *blockedCheckout) {
	r.checkoutMu.Lock()
	defer r.checkoutMu.Unlock()
	_, err := recoverPendingHarness(b.root)
	r.mu.Lock()
	r.blocked = slices.DeleteFunc(r.blocked, func(e *blockedCheckout) bool { return e == b })
	r.mu.Unlock()
	r.settleBlocked(b.info, b.root, err)
}

// exitConfirmed says whether the agent process has exited: Close reaped it. A Close
// error beside a reaped process (os/exec's ErrWaitDelay, when an orphaned
// grandchild still holds the agent's stdout) is about that pipe, not the agent, and
// is recorded on the run as a note; it does not keep a checkout held. Whatever
// outlives the agent is the process-group problem, which is not this check's.
func exitConfirmed(exit codingagent.Exit) bool { return exit.Reaped }

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
