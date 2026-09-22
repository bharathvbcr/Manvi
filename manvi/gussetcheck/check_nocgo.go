//go:build !cgo

package gussetcheck

import "context"

func check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrNotLinked
}
