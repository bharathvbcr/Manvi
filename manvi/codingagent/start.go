package codingagent

import "errors"

// The marker can only be constructed at pre-spawn boundaries in this package.
// Unknown adapter errors cannot release a durable launch claim by assertion.
type beforeStartError struct{ cause error }

func (e *beforeStartError) Error() string { return e.cause.Error() }
func (e *beforeStartError) Unwrap() error { return e.cause }
func beforeStart(err error) error         { return &beforeStartError{cause: err} }

// NoProcessStarted reports a failure from a boundary that never successfully
// started a provider. It is not a termination claim about a started process.
func NoProcessStarted(err error) bool {
	var failure *beforeStartError
	return errors.As(err, &failure) && failure != nil
}
