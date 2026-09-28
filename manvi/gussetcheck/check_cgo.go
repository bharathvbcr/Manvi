//go:build cgo

package gussetcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gussetfn"
	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/policy"
	"github.com/bharathvbcr/gusset"
)

func selfTest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gusset.MaxPoolSize < 4 {
		return fmt.Errorf("gusset: MaxPoolSize %d is below the bridge pool of 4", gusset.MaxPoolSize)
	}
	return classify(gussetfn.SelfTest(ctx))
}

func closeEngine() error {
	return gussetfn.Close()
}

func shutdownEngine(drain time.Duration) error {
	return gussetfn.Shutdown(drain)
}

func drainLogs(w io.Writer) (int, error) {
	return gussetfn.DrainLogs(w)
}

func classify(err error) error {
	if errors.Is(err, gusset.ErrPanic) || errors.Is(err, gusset.ErrPoisoned) {
		return fmt.Errorf("manvi: gusset engine poisoned the handle: %w", err)
	}
	return err
}

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gusset.MaxPoolSize < 4 {
		return fmt.Errorf("gusset: MaxPoolSize %d is below the bridge pool of 4", gusset.MaxPoolSize)
	}
	return classify(gussetfn.Check(ctx))
}

// Matcher is the engine as a policy.Matcher: every gate this build hands it
// to asks dc-glob across the boundary, and an engine failure is a hard
// denial under path.engine_unavailable or command.engine_unavailable.
func Matcher() policy.Matcher { return gussetfn.Matcher{} }
