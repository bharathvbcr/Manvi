package computer

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const attachmentAttempts = 32
const attachmentBackoff = 250 * time.Millisecond
const attachmentTimeout = 10 * time.Second

// AttachmentReadiness reports discovery attempts, without retaining native
// titles, input values, or arbitrary error messages in diagnostics.
type AttachmentReadiness struct {
	Attempts          int    `json:"attempts"`
	LastTransientCode string `json:"last_transient_code,omitempty"`
}

// AttachFocusedReady waits for the selected application's window to become
// available. It retries only explicit not_sent discovery failures with no
// attached session, for at most 32 attempts within ten seconds. The caller's
// earlier cancellation/deadline wins. A successful or uncertain attachment is
// never retried; callers must retire the client on an uncertain error.
func (c *Client) AttachFocusedReady(ctx context.Context, runID string, pid uint32, readOnly []Selector) (Session, AttachmentReadiness, error) {
	ctx, cancel := context.WithTimeout(ctx, attachmentTimeout)
	defer cancel()
	// Freeze the trusted form-input allowance across the whole admission.
	readOnly = ownedSelectors(readOnly)
	return waitForAttachment(ctx, func(ctx context.Context) (Session, error) {
		return c.AttachFocused(ctx, runID, pid, readOnly)
	}, attachmentAttempts, attachmentBackoff)
}

func waitForAttachment(ctx context.Context, attach func(context.Context) (Session, error), limit int, backoff time.Duration) (Session, AttachmentReadiness, error) {
	var diagnostics AttachmentReadiness
	for {
		if err := ctx.Err(); err != nil {
			return Session{}, diagnostics, err
		}
		diagnostics.Attempts++
		s, err := attach(ctx)
		if err == nil {
			return s, diagnostics, nil
		}
		if ctx.Err() != nil {
			return s, diagnostics, ctx.Err()
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
		case <-ctx.Done():
			timer.Stop()
			return s, diagnostics, ctx.Err()
		case <-timer.C:
		}
	}
}
