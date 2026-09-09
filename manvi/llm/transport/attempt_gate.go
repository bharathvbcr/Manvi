package transport

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// AttemptInfo identifies an actual HTTP attempt, including retries. It carries
// no request body, credentials or query parameters. A caller can capture the
// model and its conservative price bound in its per-request gate closure.
type AttemptInfo struct {
	Provider     string
	Method       string
	Path         string
	Number       int
	RequestBytes int
}

type AttemptState string

const (
	AttemptNotSent  AttemptState = "not_sent"
	AttemptRejected AttemptState = "rejected"
	AttemptAccepted AttemptState = "accepted"
	AttemptUnknown  AttemptState = "unknown"
)

// AttemptResult describes transport admission, not final billed usage.
// Only not_sent proves that no HTTP send occurred. Accepted and rejected
// attempts can still have unknown charges; reconcile from provider usage or
// retain their reservation conservatively after cancellation or a lost stream.
type AttemptResult struct {
	State     AttemptState
	Status    int
	RequestID string
}

// AttemptGate reserves permission/budget before an HTTP attempt. Its optional
// callback runs exactly once for an admitted attempt, after transport/preflight
// classification. A callback error aborts further retries. Both functions must
// finish within the caller's deadline; a reservation must never be refunded
// merely because that deadline elapsed.
type AttemptGate func(context.Context, AttemptInfo) (func(AttemptResult) error, error)

type attemptGateKey struct{}

// WithAttemptGate applies an attempt gate to one request tree without mutating
// a shared adapter. A nil gate explicitly disables a inherited gate.
func WithAttemptGate(ctx context.Context, gate AttemptGate) context.Context {
	return context.WithValue(ctx, attemptGateKey{}, gate)
}

func (c *Client) admitAttempt(ctx context.Context, method, path string, payload []byte, number int) (func(AttemptResult) error, *Error) {
	gate, _ := ctx.Value(attemptGateKey{}).(AttemptGate)
	if gate == nil {
		return nil, nil
	}
	finish, err := gate(ctx, AttemptInfo{Provider: c.Provider, Method: method, Path: strings.SplitN(path, "?", 2)[0], Number: number, RequestBytes: len(payload)})
	if err != nil {
		return nil, &Error{Provider: c.Provider, Attempts: number - 1, permanent: true, notSent: true, Err: fmt.Errorf("attempt admission: %w", err)}
	}
	return finish, nil
}

func (c *Client) finishAttempt(finish func(AttemptResult) error, resp *http.Response, failure *Error, number int) *Error {
	if finish == nil {
		return nil
	}
	result := AttemptResult{State: AttemptUnknown}
	if resp != nil {
		result.State = AttemptAccepted
		result.Status = resp.StatusCode
		result.RequestID = resp.Header.Get(c.RequestIDHeader)
	}
	if failure != nil {
		result.Status = failure.Status
		result.RequestID = failure.RequestID
		switch {
		case failure.notSent:
			result.State = AttemptNotSent
		case failure.inStream:
			result.State = AttemptUnknown
		case failure.Status != 0:
			result.State = AttemptRejected
		default:
			result.State = AttemptUnknown
		}
	}
	if err := finish(result); err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		return &Error{Provider: c.Provider, Attempts: number, permanent: true, Err: fmt.Errorf("attempt accounting: %w", err)}
	}
	return nil
}

// AttemptGatedProvider explicitly promises that every outbound provider HTTP
// attempt passes through this package's admission gate. Budgeted callers reject
// providers without this contract before invoking Stream.
type AttemptGatedProvider interface {
	AttemptGateSupported() bool
}
