//go:build !cgo

package gussetcheck

import (
	"context"
	"errors"
	"testing"
)

func TestRunReportsEngineNotLinked(t *testing.T) {
	err := Run(context.Background())
	if !errors.Is(err, ErrNotLinked) {
		t.Fatalf("Run() = %v, want ErrNotLinked", err)
	}
	if got := Ready(); !errors.Is(got, ErrNotLinked) {
		t.Fatalf("Ready() = %v, want ErrNotLinked", got)
	}
}

func TestSelfTestAndCloseWithoutTheEngine(t *testing.T) {
	if err := SelfTest(context.Background()); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("SelfTest() = %v, want ErrNotLinked", err)
	}
	if err := Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}
}

func TestRunPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run() = %v, want context.Canceled", err)
	}
}
