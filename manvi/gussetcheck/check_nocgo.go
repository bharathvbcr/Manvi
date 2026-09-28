//go:build !cgo

package gussetcheck

import (
	"context"
	"time"
)

func selfTest(ctx context.Context) error {
	return check(ctx)
}

func closeEngine() error { return nil }

func shutdownEngine(time.Duration) error { return nil }

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrNotLinked
}
