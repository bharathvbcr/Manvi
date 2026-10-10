package tui

import (
	"testing"
	"time"
)

// An idle terminal must not repaint on the 100ms tick: the frame would be the
// same cells, drawn ten times a second for as long as the operator reads.
func TestIdleTickDoesNotMoveTheFrame(t *testing.T) {
	a, _ := newTestApp()
	for i := 0; i < 120; i++ {
		a.Dispatch(ActionTick{})
		// The composer's placeholder tip rotates every fifty ticks, and is the
		// one thing an idle session animates.
		want := (i+1)%50 == 0
		if got := a.TickMoved(); got != want {
			t.Fatalf("idle tick %d: TickMoved = %v, want %v", i+1, got, want)
		}
	}

	// A draft in the composer has no placeholder to rotate.
	typeText(a, "draft")
	for i := 0; i < 100; i++ {
		a.Dispatch(ActionTick{})
		if a.TickMoved() {
			t.Fatalf("tick %d with a draft typed moved the frame", i)
		}
	}
}

func TestTickMovesTheFrameWhileSomethingAnimates(t *testing.T) {
	t.Run("a running turn", func(t *testing.T) {
		a, _ := newTestApp()
		typeText(a, "x")
		a.Dispatch(ActionTurnStarted{SessionID: "S1"})
		a.Dispatch(ActionTick{})
		if !a.TickMoved() {
			t.Fatal("a busy session's spinner did not move on the tick")
		}
		a.Dispatch(ActionTurnEnded{SessionID: "S1"})
		a.Dispatch(ActionTick{})
		if a.TickMoved() {
			t.Fatal("the tick moved the frame once the turn had ended")
		}
	})
	t.Run("a notice counting down, and the tick that clears it", func(t *testing.T) {
		a, _ := newTestApp()
		typeText(a, "x")
		a.setNotice("hello", StatusInfo)
		a.Dispatch(ActionTick{})
		if !a.TickMoved() {
			t.Fatal("a notice was on screen and the tick did not move the frame")
		}
		a.noticeAt = time.Now().Add(-7 * time.Second)
		a.Dispatch(ActionTick{})
		if !a.TickMoved() {
			t.Fatal("the tick that cleared the notice did not repaint, so it stayed on screen")
		}
		if a.Notice(); a.notice != "" {
			t.Fatal("the notice outlived its six seconds")
		}
		a.Dispatch(ActionTick{})
		if a.TickMoved() {
			t.Fatal("the tick after the notice cleared still moved the frame")
		}
	})
	t.Run("the dashboard", func(t *testing.T) {
		a, _ := newTestApp()
		a.mode = ModeDashboard
		a.Dispatch(ActionTick{})
		if !a.TickMoved() {
			t.Fatal("the dashboard's sweep did not move on the tick")
		}
	})
	t.Run("the empty screen's rain", func(t *testing.T) {
		a := NewApp(Dark(), &stubHost{})
		a.Dispatch(ActionTick{})
		if got, want := a.TickMoved(), a.fxOn(); got != want {
			t.Fatalf("empty screen TickMoved = %v, want %v (animation gate)", got, want)
		}
	})
}
