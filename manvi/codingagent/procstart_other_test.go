//go:build !linux && !darwin

package codingagent

import (
	"errors"
	"os"
	"testing"
)

func TestProcessStartSaysItCannotTellWhereThePlatformCannot(t *testing.T) {
	if _, err := ProcessStart(os.Getpid()); !errors.Is(err, ErrProcessStartUnsupported) {
		t.Fatalf("ProcessStart = %v, want ErrProcessStartUnsupported", err)
	}
}
