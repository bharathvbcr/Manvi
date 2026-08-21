package tui

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newLoopless builds a Runner with only the fields the turn bookkeeping uses.
// tui.New refuses to build one without a real terminal, and the bookkeeping is
// the part with a rule in it.
func newLoopless() *Runner {
	return &Runner{
		actions: make(chan Action, 16),
		cancels: map[string]context.CancelFunc{},
	}
}

// drain collects the actions a runner queued, without a loop to consume them.
func drain(r *Runner) []Action {
	var out []Action
	for {
		select {
		case a := <-r.actions:
			out = append(out, a)
		case <-time.After(200 * time.Millisecond):
			return out
		}
	}
}

// TestASecondTurnIsRefusedOutLoud.
//
// One turn per session is the right rule — two would interleave their writes
// into a single session log, and that log is the only thing the model's history
// is projected from. Enforcing it by returning in silence is not. The composer
// queues a prompt typed during a turn and says so on the shortcut bar; a slash
// command took a different path, arrived here, and vanished, so the operator
// saw the command echo and no result ever follow.
func TestASecondTurnIsRefusedOutLoud(t *testing.T) {
	r := newLoopless()
	release := make(chan struct{})
	r.startTurn(context.Background(), "S1", func(context.Context) error {
		<-release
		return nil
	})

	r.startTurn(context.Background(), "S1", func(context.Context) error {
		t.Error("the second turn ran")
		return nil
	})

	close(release)
	var started, noticed int
	for _, act := range drain(r) {
		switch a := act.(type) {
		case ActionTurnStarted:
			started++
		case ActionNotice:
			noticed++
			if a.SessionID != "S1" {
				t.Errorf("the refusal was reported against %q, not the session it happened in", a.SessionID)
			}
			if a.Status != StatusWarn {
				t.Errorf("the refusal was reported at %v, want a warning", a.Status)
			}
		}
	}
	if started != 1 {
		t.Errorf("%d turns started, want 1", started)
	}
	if noticed != 1 {
		t.Errorf("%d refusals reported, want 1 — a dropped command must not be silent", noticed)
	}
}

// TestBusyExcludesTheCallersOwnTurn.
//
// A slash command is itself run as a turn, so a caller asking "is anything
// running" without discounting itself would always be told yes — and the guard
// that uses this answer would refuse every settings change there has ever been.
func TestBusyExcludesTheCallersOwnTurn(t *testing.T) {
	r := newLoopless()
	release := make(chan struct{})
	for _, id := range []string{"S1", "S2", "S3"} {
		r.startTurn(context.Background(), id, func(context.Context) error {
			<-release
			return nil
		})
	}

	if got := r.Busy("S1"); len(got) != 2 || got[0] != "S2" || got[1] != "S3" {
		t.Fatalf("Busy(S1) = %v, want the other two sessions in order", got)
	}
	if got := r.Busy(""); len(got) != 3 {
		t.Fatalf("Busy(\"\") = %v, want all three", got)
	}
	close(release)

	// And it empties as the turns end. The deferred cleanup runs on each
	// turn's own goroutine, so this waits rather than asserting immediately.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.Busy("")) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("turns still registered as running after they finished: %v", r.Busy(""))
}

// TestATurnThatFailsStillClearsItsSlot. A session that stayed permanently
// "busy" after an error would refuse every later command with a notice about a
// turn that is not running.
func TestATurnThatFailsStillClearsItsSlot(t *testing.T) {
	r := newLoopless()
	r.startTurn(context.Background(), "S1", func(context.Context) error {
		return errors.New("the harness refused")
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(r.Busy("")) == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("a failed turn left its session marked busy for ever")
}
