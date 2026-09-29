//go:build !cgo

package gussetcheck

import (
	"context"
	"io"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
)

func selfTest(ctx context.Context) error {
	return check(ctx)
}

func closeEngine() error { return nil }

func shutdownEngine(time.Duration) error { return nil }

func drainLogs(io.Writer) (int, error) { return 0, nil }

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrNotLinked
}

// Matcher is nil without the engine: a gate given nil matches with fnmatch,
// the answer the engine is held equal to.
func Matcher() policy.Matcher { return nil }
