//go:build !cgo

package gussetcheck

import (
	"context"
	"io"
	"time"
)

func selfTest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrNotLinked
}

func shutdownEngine(time.Duration) error { return nil }

func drainLogs(io.Writer) (int, error) { return 0, nil }
