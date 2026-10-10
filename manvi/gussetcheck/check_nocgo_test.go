//go:build !cgo

package gussetcheck

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSelfTestAndShutdownWithoutTheEngine(t *testing.T) {
	if err := SelfTest(context.Background()); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("SelfTest() = %v, want ErrNotLinked", err)
	}
	if err := Shutdown(time.Second); err != nil {
		t.Fatalf("Shutdown() = %v, want nil", err)
	}
}

func TestSelfTestPreservesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := SelfTest(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("SelfTest() = %v, want context.Canceled", err)
	}
}
