package codingagent

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"unicode/utf8"
)

// A managed session takes its first turn from StartTurn and every later one
// from ContinueTurn.
//
// Both providers keep the process alive between turns, and that is the whole
// mechanism; nothing here relaunches or resumes. Measured against the installed
// CLIs rather than assumed from their documentation:
//
//   - Claude Code 2.1.316: a second `user` frame written to the same
//     `--input-format stream-json` process after a `result` starts another turn
//     in the same session. The CLI answers it with a second `system/init`
//     (identical cwd, permission mode, model and build) and a second `result`,
//     and the new turn's first assistant message carries a new id.
//   - codex-cli 0.153.4: a second `turn/start` for the same `threadId` starts
//     another turn on the same thread, including a thread opened with
//     `ephemeral: true`. It is answered with a new turn id, and `turn/started` /
//     `turn/completed` carry it. `turn/steer` is not this path: it needs an
//     active turn, which a continuation refuses by definition.
//
// The rules a continuation keeps are the ones StartTurn keeps, stated once here:
//
//   - A refusal wrote nothing. Every precondition failure is a refusal (see
//     ContinueRefused), so a caller can retry it after fixing the cause.
//   - Any other error means a frame may have reached the provider. The session
//     is halted: it never sends another follow-up, because a resend could run
//     the same instruction twice. The caller records the delivery as unresolved
//     (see DeliveryUnresolved) and ends the run.
//   - Bounded in size, in turns per session, and one at a time.
const (
	// MaxContinueMessageBytes bounds one follow-up. It is far under the 128 KiB
	// task brief because the run's saved output is a single 128 KiB record shared
	// with the turn text, and the host writes a claim into it before sending.
	MaxContinueMessageBytes = 32 * 1024

	// MaxContinuations bounds the follow-up turns one session takes after its
	// first. A host may ask for fewer; it cannot ask for more.
	MaxContinuations = 8
)

var (
	// ErrContinueUnsupported means this provider or session cannot take a
	// follow-up turn. It is the typed refusal for a provider without the
	// mechanism above; both shipped adapters have it.
	ErrContinueUnsupported = errors.New("this managed provider cannot take a follow-up turn")
	// ErrTurnInFlight means a turn is still running, or the session is busy reading
	// one, or events or callbacks of the finished turn are still unread.
	ErrTurnInFlight = errors.New("a turn is still in flight, or events of the last one are unread")
	// ErrNoCompletedTurn means there is no turn that completed to follow: none was
	// started, or the last one failed or was interrupted.
	ErrNoCompletedTurn = errors.New("a follow-up needs a session whose previous turn completed")
	// ErrSessionClosed means the session was closed or its process is gone.
	ErrSessionClosed = errors.New("the managed session is closed")
	// ErrContinueLimit means the session took the most turns it may.
	ErrContinueLimit = errors.New("the session already took the most turns it may")
	// ErrContinueMessage means the follow-up is blank, not valid UTF-8, or past
	// MaxContinueMessageBytes.
	ErrContinueMessage = errors.New("a follow-up is one nonblank message within its size bound")
	// ErrContinuationHalted refuses a follow-up on a session whose earlier
	// follow-up has an unresolved delivery.
	ErrContinuationHalted = errors.New("an earlier follow-up's delivery is unresolved, so no further follow-up is sent")
	// ErrDeliveryUnresolved is what an ambiguous send wraps: a frame may or may
	// not have reached the provider, and nothing here will send it again.
	ErrDeliveryUnresolved = errors.New("follow-up delivery is unresolved; it will not be sent again")
)

// continueRefusal marks a ContinueTurn failure from a boundary that wrote
// nothing.
type continueRefusal struct{ cause error }

func (e *continueRefusal) Error() string { return e.cause.Error() }
func (e *continueRefusal) Unwrap() error { return e.cause }

// RefuseContinue marks err as a refusal: the caller asserts that nothing was
// written to the provider. The adapters use it for every precondition; a
// ManagedSession implemented elsewhere uses it for the same and for a provider
// without the mechanism (wrapping ErrContinueUnsupported). It is a statement
// about the write, so it is never applied to an error from a send that began.
func RefuseContinue(err error) error { return &continueRefusal{cause: err} }

// ContinueRefused reports a ContinueTurn error that wrote nothing to the
// provider, so the same follow-up may be offered again once its cause is gone.
// Any other non-nil error from ContinueTurn is DeliveryUnresolved.
func ContinueRefused(err error) bool {
	var refusal *continueRefusal
	return errors.As(err, &refusal) && refusal != nil
}

// unresolvedDelivery wraps the error of a send that may have reached the
// provider.
type unresolvedDelivery struct{ cause error }

func (e *unresolvedDelivery) Error() string {
	return ErrDeliveryUnresolved.Error() + ": " + e.cause.Error()
}
func (e *unresolvedDelivery) Unwrap() []error { return []error{ErrDeliveryUnresolved, e.cause} }

// DeliveryUnresolved reports a ContinueTurn error after which the follow-up may
// or may not have been delivered. It is never retried.
func DeliveryUnresolved(err error) bool { return errors.Is(err, ErrDeliveryUnresolved) }

// ContinuableProviders are the providers whose adapter implements
// ContinueTurn. A provider absent from it has one turn per process.
var ContinuableProviders = []string{"codex", "claude"}

// SupportsContinue reports whether a stored run's provider can take follow-ups.
func SupportsContinue(provider string) bool {
	for _, known := range ContinuableProviders {
		if known == provider {
			return true
		}
	}
	return false
}

// turnLedger is the turn accounting both adapters share, so the preconditions
// of a follow-up are one predicate and cannot drift between providers. Its
// fields other than closed are guarded by the owning adapter's mutex.
type turnLedger struct {
	// turns counts the turns started on this session, the first included.
	turns int
	// last is the status the most recent turn finished with.
	last string
	// halted is set when a follow-up's delivery was ambiguous.
	halted bool
	// closed is set by Close without the adapter's mutex, which a blocked read
	// holds.
	closed atomic.Bool
}

// admit decides whether a follow-up may be written now. Every error it returns
// is a refusal. busy is whether anything of the previous turn is still unread
// or unanswered.
func (l *turnLedger) admit(ctx context.Context, message string, ready, started, finished, busy bool, wire *connection) error {
	if l.closed.Load() || wire.ctx.Err() != nil {
		return RefuseContinue(ErrSessionClosed)
	}
	select {
	case <-wire.done:
		return RefuseContinue(ErrSessionClosed)
	default:
	}
	switch {
	case l.halted:
		return RefuseContinue(ErrContinuationHalted)
	case ctx.Err() != nil:
		return RefuseContinue(ctx.Err())
	case strings.TrimSpace(message) == "" || len(message) > MaxContinueMessageBytes || !utf8.ValidString(message) || strings.ContainsRune(message, 0):
		return RefuseContinue(ErrContinueMessage)
	case !ready || !started:
		return RefuseContinue(ErrNoCompletedTurn)
	case !finished || busy:
		return RefuseContinue(ErrTurnInFlight)
	case l.last != "completed":
		return RefuseContinue(ErrNoCompletedTurn)
	case l.turns >= 1+MaxContinuations:
		return RefuseContinue(ErrContinueLimit)
	}
	return nil
}
