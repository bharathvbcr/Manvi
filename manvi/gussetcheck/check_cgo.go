//go:build cgo

package gussetcheck

import (
	"context"
	"errors"
	"fmt"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/gussetfn"
	"github.com/bharathvbcr/gusset"
)

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gusset.MaxPoolSize < 4 {
		return fmt.Errorf("gusset: MaxPoolSize %d is below the bridge pool of 4", gusset.MaxPoolSize)
	}
	err := gussetfn.Check(ctx)
	if errors.Is(err, gusset.ErrPanic) || errors.Is(err, gusset.ErrPoisoned) {
		return fmt.Errorf("manvi: gusset engine poisoned the handle: %w", err)
	}
	return err
}
