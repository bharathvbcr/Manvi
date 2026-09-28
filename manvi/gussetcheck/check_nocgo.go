//go:build !cgo

package gussetcheck

import "context"

func selfTest(ctx context.Context) error {
	return check(ctx)
}

func closeEngine() error { return nil }

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrNotLinked
}
