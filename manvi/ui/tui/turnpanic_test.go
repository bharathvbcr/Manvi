package tui

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A turn runs on its own goroutine, where nothing above it can recover. A panic
// there used to end the whole process with the terminal raw on the alternate
// screen, the session unsaved and every lease held. It is now an error on the
// turn: the session ends the turn as it ends any failed one and stays usable,
// and the stack is kept to be written once the terminal is back.
func TestAPanickingTurnIsAnErrorNotACrash(t *testing.T) {
	r := newLoopless()
	r.startTurn(context.Background(), "S1", func(context.Context) error {
		panic("provider adapter: index out of range [3] with length 2")
	})

	var ended *ActionTurnEnded
	deadline := time.After(5 * time.Second)
	for ended == nil {
		select {
		case act := <-r.actions:
			if e, ok := act.(ActionTurnEnded); ok {
				ended = &e
			}
		case <-deadline:
			t.Fatal("the panicking turn never reported its end")
		}
	}
	if ended.Err == nil || !strings.Contains(ended.Err.Error(), "internal error") ||
		!strings.Contains(ended.Err.Error(), "index out of range") {
		t.Fatalf("the turn ended with %v, want an internal error naming the panic", ended.Err)
	}

	// The slot is free for the next turn.
	deadline2 := time.Now().Add(2 * time.Second)
	for len(r.Busy("")) != 0 {
		if time.Now().After(deadline2) {
			t.Fatal("a panicked turn left its session marked busy")
		}
		time.Sleep(5 * time.Millisecond)
	}

	r.mu.Lock()
	reports := append([]string(nil), r.panics...)
	r.mu.Unlock()
	if len(reports) != 1 || !strings.Contains(reports[0], "index out of range") ||
		!strings.Contains(reports[0], "TestAPanickingTurnIsAnErrorNotACrash") {
		t.Fatalf("the panic report lost its value or its stack: %q", reports)
	}
}
