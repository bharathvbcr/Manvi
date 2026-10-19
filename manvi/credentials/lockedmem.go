package credentials

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// ErrLockedMemory reports that a credential could not be sealed or opened
// because the process could not get locked memory, or because an earlier
// memguard call never returned.
//
// It exists because of how memguard v0.23.0 answers a failed mlock: core.NewBuffer
// calls core.Panic, whose Purge takes the key mutex and then the key coffer's
// mutex. Two of its allocations happen with one of those already held —
// getOrCreateKey holds the key mutex across NewCoffer, and Coffer.View holds
// the coffer across its NewBuffer — and Go mutexes are not reentrant, so the
// calling goroutine blocks on itself, forever, with memguard's global locks
// held. Every later memguard call in the process then blocks behind it. Where
// no lock is held (Enclave.Open's buffer, NewBuffer for the scrubber) the Purge
// completes and then panics, having destroyed every guarded buffer in the
// process and rotated the key, so every sealed credential is unrecoverable and
// the scrubber's views read as empty strings.
//
// Neither outcome is an error a caller can handle, so this package does not
// let memguard reach it: see guarded. The protection is not relaxed — nothing
// is ever held in unlocked memory instead. A credential that cannot be locked
// is a credential this process refuses to hold.
var ErrLockedMemory = errors.New("credentials: locked memory is unavailable")

// memguardBound is the longest one memguard call may take before the caller is
// told it failed. A healthy call takes microseconds; this is generous enough
// that a machine under heavy load does not trip it, and short enough that a
// deadlocked one is reported rather than discovered by a test timeout. It is a
// variable so the watchdog test can shorten it.
var memguardBound = 10 * time.Second

// memguardSlot serialises this package's memguard allocations. The probe in
// guarded is only meaningful if nothing else in the process consumes locked
// memory between the probe and the call; this package is the only importer of
// memguard in the module, so serialising here closes that window for every
// in-process caller. A channel rather than a mutex because acquiring it must
// itself be bounded: a slot held by a deadlocked call is never released.
var memguardSlot = make(chan struct{}, 1)

// memguardStuck counts guarded calls that overran memguardBound and have not
// returned since. While it is non-zero, memguard's global locks are probably
// held by a goroutine that will never release them, and a new call would only
// join it, so guarded refuses at once.
var memguardStuck atomic.Int64

// Page budgets for the probe. Only the pages between memguard's guard pages
// are mlocked, rounded up to whole pages: NewBuffer(n) locks pagesFor(n).
const (
	// coldKeyPages is the key coffer's three 32-byte buffers, allocated on the
	// first enclave operation of the process, or after a Purge.
	coldKeyPages = 3
	// keyViewPages is the 32-byte copy of the key every seal and open makes.
	keyViewPages = 1
	// enclavePages is what sealing or opening costs on top of any plaintext
	// buffer: the key view, plus a cold key in case this is the first call.
	// Over-probing costs a few pages for a few microseconds; under-probing is
	// the deadlock.
	enclavePages = coldKeyPages + keyViewPages
)

// pagesFor is how many pages memguard locks to hold n bytes.
func pagesFor(n int) int {
	page := os.Getpagesize()
	return (n + page - 1) / page
}

// guarded runs one memguard call that allocates locked memory, so that it
// returns an error instead of deadlocking or purging.
//
// First it checks that the pages the call will lock can be locked right now,
// by locking and releasing that many itself; if they cannot, memguard is never
// called. That turns every deterministic case — an RLIMIT_MEMLOCK too low, a
// wired-memory ceiling already reached — into ErrLockedMemory before memguard
// can panic. What remains is a race: locked memory taken by another process
// between the probe and the call. For that the call runs on its own goroutine
// and the caller waits at most memguardBound, then gets ErrLockedMemory and the
// package refuses further calls until the overrunning one returns. If it never
// does, that goroutine is leaked with memguard's locks — nothing in this
// process can release them — and the refusal is permanent, which is correct.
//
// A panic inside op is re-raised on the caller's goroutine, not converted to
// an error. Reaching one means memguard completed a Purge: the key has rotated
// and every guarded buffer in the process is gone, the scrubber's included,
// and carrying on would mean scrubbing nothing while reporting that it works.
//
// discard releases a result that arrives after the caller stopped waiting.
func guarded[T any](pages int, op func() T, discard func(T)) (T, error) {
	var zero T
	if n := memguardStuck.Load(); n > 0 {
		return zero, fmt.Errorf("%w: %d earlier memguard call(s) have not returned after %s", ErrLockedMemory, n, memguardBound)
	}
	deadline := time.NewTimer(memguardBound)
	defer deadline.Stop()
	select {
	case memguardSlot <- struct{}{}:
	case <-deadline.C:
		return zero, fmt.Errorf("%w: waited %s for another memguard call to return", ErrLockedMemory, memguardBound)
	}
	if err := probeLockedMemory(pages); err != nil {
		<-memguardSlot
		return zero, fmt.Errorf("%w: %d page(s) could not be locked: %w", ErrLockedMemory, pages, err)
	}

	type outcome struct {
		value    T
		panicked bool
		panicVal any
	}
	var (
		mu        sync.Mutex
		abandoned bool
		done      = make(chan outcome, 1)
	)
	go func() {
		defer func() { <-memguardSlot }()
		var out outcome
		func() {
			defer func() {
				if p := recover(); p != nil {
					out = outcome{panicked: true, panicVal: p}
				}
			}()
			out.value = op()
		}()
		mu.Lock()
		if !abandoned {
			done <- out
			mu.Unlock()
			return
		}
		memguardStuck.Add(-1)
		mu.Unlock()
		if out.panicked {
			// No caller is left to re-raise it to, and the process is in the
			// state described above. Crash it rather than run on.
			panic(out.panicVal)
		}
		if discard != nil {
			discard(out.value)
		}
	}()

	var out outcome
	select {
	case out = <-done:
	case <-deadline.C:
		mu.Lock()
		select {
		case out = <-done:
			// It finished as the deadline fired; take the result.
			mu.Unlock()
		default:
			abandoned = true
			memguardStuck.Add(1)
			mu.Unlock()
			return zero, fmt.Errorf("%w: a memguard call did not return within %s", ErrLockedMemory, memguardBound)
		}
	}
	if out.panicked {
		panic(out.panicVal)
	}
	return out.value, nil
}
