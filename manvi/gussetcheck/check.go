// Package gussetcheck is Manvi's caller for the shared Gusset engine.
//
// The engine archive is DevCouncil's umbrella (one staticlib in this
// process, R14). Manvi does not link a second one. A panic from the
// diagnostic engine is reported as a failure, not treated as a match.
package gussetcheck

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gussetfn"
	"github.com/bharathvbcr/gusset"
)

// Run checks the linked engine. The pool ceiling is read from Gusset so a
// build that imported a different module fails here instead of at a later call.
func Run(ctx context.Context) error {
	if gusset.MaxPoolSize < 4 {
		return fmt.Errorf("gusset: MaxPoolSize %d is below the bridge pool of 4", gusset.MaxPoolSize)
	}
	err := gussetfn.Check(ctx)
	if errors.Is(err, gusset.ErrPanic) || errors.Is(err, gusset.ErrPoisoned) {
		return fmt.Errorf("manvi: gusset engine poisoned the handle: %w", err)
	}
	return err
}

var (
	readyOnce sync.Once
	readyErr  error
)

// Ready runs the engine check once per process. A failure stays failed:
// a poisoned handle does not start answering policy checks on the next call.
func Ready() error {
	readyOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		readyErr = Run(ctx)
	})
	return readyErr
}
