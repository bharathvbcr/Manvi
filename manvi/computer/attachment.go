package computer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const attachmentAttempts = 32
const attachmentBackoff = 250 * time.Millisecond

// attachmentTimeout is the window in which a new attempt may start. It does not
// cut off an attempt already sent: an attach cancelled after it reaches the
// broker retires the broker (it may have focused the window, see readOnlyOps),
// and the broker's own helper can take up to 12 s (HELPER_TIMEOUT in
// native/crates/manvi-desktop/src/broker.rs), longer than this window. Cutting
// at the window therefore turned an application whose tree never settled into a
// dead broker instead of an accessibility_pending answer.
//
// attachmentCallGrace bounds the attempt that is still running when the window
// closes. It is longer than the helper limit, so reaching it means the broker
// stopped answering, and retiring it is then the right outcome. A variable, not a
// constant, only so the tests can shorten both.
var (
	attachmentTimeout   = 10 * time.Second
	attachmentCallGrace = 15 * time.Second
)

// AttachmentReadiness reports discovery attempts, without retaining native
// titles, input values, or arbitrary error messages in diagnostics.
type AttachmentReadiness struct {
	Attempts          int    `json:"attempts"`
	LastTransientCode string `json:"last_transient_code,omitempty"`
}

// AttachFocusedReady waits for the selected application's window to become
// available. It retries only explicit not_sent discovery failures with no
// attached session, for at most 32 attempts started within ten seconds; an
// attempt in flight when that window closes is allowed to finish, so the whole
// call returns within 25 seconds. A caller's own cancellation or deadline still
// interrupts an attempt in flight, which retires the broker, so a caller should
// not give this call a deadline of its own shorter than that bound. A successful
// or uncertain attachment is never retried; callers must retire the client on an
// uncertain error.
func (c *Client) AttachFocusedReady(ctx context.Context, runID string, pid uint32, readOnly []Selector) (Session, AttachmentReadiness, error) {
	call, cancelCall := context.WithTimeout(ctx, attachmentTimeout+attachmentCallGrace)
	defer cancelCall()
	window, cancelWindow := context.WithTimeout(call, attachmentTimeout)
	defer cancelWindow()
	// Freeze the trusted form-input allowance across the whole admission.
	readOnly = ownedSelectors(readOnly)
	return waitForAttachment(window, call, func(ctx context.Context) (Session, error) {
		return c.AttachFocused(ctx, runID, pid, readOnly)
	}, attachmentAttempts, attachmentBackoff)
}

// waitForAttachment starts attempts while window is open and runs each one
// under call, which window is derived from. So the window running out ends the
// loop between attempts, after the last attempt's reason is recorded, and only
// call ending (the caller's cancellation, or the grace past the window) stops an
// attempt that is already with the broker.
func waitForAttachment(window, call context.Context, attach func(context.Context) (Session, error), limit int, backoff time.Duration) (Session, AttachmentReadiness, error) {
	var diagnostics AttachmentReadiness
	for {
		if err := window.Err(); err != nil {
			return Session{}, diagnostics, budgetError(err, diagnostics)
		}
		diagnostics.Attempts++
		s, err := attach(call)
		if err == nil {
			return s, diagnostics, nil
		}
		if call.Err() != nil {
			// The attempt was cut off with the broker holding it. Its error says
			// so (the context error joined with the broker's retirement); keep
			// it, and say which attempt and why the one before it was retried.
			return s, diagnostics, fmt.Errorf("%w: attempt %d was interrupted: %w", budgetError(call.Err(), diagnostics), diagnostics.Attempts, err)
		}
		var native *BrokerError
		if s.ID != "" || s.Epoch != 0 || !errors.As(err, &native) || native.Delivery != "not_sent" ||
			(native.Code != "window_missing" && native.Code != "window_ambiguous" && native.Code != "accessibility_pending") {
			return s, diagnostics, err
		}
		diagnostics.LastTransientCode = native.Code
		if diagnostics.Attempts >= limit {
			return s, diagnostics, fmt.Errorf("native attachment readiness exhausted after %d attempts: %w", diagnostics.Attempts, err)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-window.Done():
			timer.Stop()
			return s, diagnostics, budgetError(window.Err(), diagnostics)
		case <-timer.C:
		}
	}
}

// budgetError keeps the context error (errors.Is still matches Canceled and
// DeadlineExceeded, and it is still not a *BrokerError) and adds why the last
// attempt was retried, so a target whose tree never settled does not surface as
// a bare "context deadline exceeded". With no transient reason there is nothing
// to add and the context error is returned as it was.
func budgetError(ctxErr error, d AttachmentReadiness) error {
	switch d.LastTransientCode {
	case "":
		return ctxErr
	case "accessibility_pending":
		// accessibility_pending is a tree that kept changing on macOS and an
		// application not yet registered with AT-SPI on Linux; both mean the
		// window's accessibility tree was not ready.
		return fmt.Errorf("native attachment not ready after %d attempt(s): the window's accessibility tree was not ready (%s): %w", d.Attempts, d.LastTransientCode, ctxErr)
	default:
		return fmt.Errorf("native attachment not ready after %d attempt(s): last reason %s: %w", d.Attempts, d.LastTransientCode, ctxErr)
	}
}
