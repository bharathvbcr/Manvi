package tui

import (
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// twoSessions builds an app with S1 and S2, S1 showing, a fake clock on it, and
// a card raised on S1 that has been painted and left alone past the quiet
// period: the state in which the next key would be a deliberate answer.
func twoSessions(t *testing.T, req ui.Request) (*App, *fakeClock, chan ui.Decision) {
	t.Helper()
	a, _ := newTestApp()
	_ = a.AddSession("S2", "session two")
	clk := newFakeClock(a)
	drawAt(a, 100, 30)
	clickTab(t, a, 0)
	if a.Current().ID != "S1" {
		t.Fatalf("setup: current is %q", a.Current().ID)
	}
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: req, Reply: reply})
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	return a, clk, reply
}

func clickTab(t *testing.T, a *App, session int) {
	t.Helper()
	drawAt(a, 100, 30)
	for _, tab := range a.tabs {
		if !tab.IsNewButton && tab.SessionIndex == session {
			a.Dispatch(ActionClick{X: tab.Rect.X + 1, Y: tab.Rect.Y, Button: 1})
			return
		}
	}
	t.Fatalf("no tab for session %d", session)
}

func clickNewSession(t *testing.T, a *App) []Effect {
	t.Helper()
	drawAt(a, 100, 30)
	for _, tab := range a.tabs {
		if tab.IsNewButton {
			return a.Dispatch(ActionClick{X: tab.Rect.X + 1, Y: tab.Rect.Y, Button: 1})
		}
	}
	t.Fatal("no new-session button")
	return nil
}

// afterReturn is the shared tail: the operator is back on the card, it is
// painted again, and a digit arrives. It must be dropped and restart the quiet
// period; only a digit after a further pause may act.
func afterReturn(t *testing.T, a *App, clk *fakeClock, reply chan ui.Decision, digit rune) []Effect {
	t.Helper()
	drawAt(a, 100, 30)
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("the card is gone")
	}
	effects := a.Dispatch(ActionRune{Runes: []rune{digit}})
	if d, ok := answered(reply); ok || len(effects) != 0 {
		t.Fatalf("the first key after returning acted on the card: %+v %v", d, effects)
	}
	if card.stage != stageChoose {
		t.Fatalf("the first key after returning moved the card to stage %v", card.stage)
	}
	// Restarted: a key short of the quiet period is still dropped.
	clk.advance(approvalQuiet - 10*time.Millisecond)
	if effects := a.Dispatch(ActionRune{Runes: []rune{digit}}); len(effects) != 0 || card.stage != stageChoose {
		t.Fatalf("the quiet period did not restart: %v stage=%v", effects, card.stage)
	}
	clk.advance(approvalQuiet + time.Millisecond)
	return a.Dispatch(ActionRune{Runes: []rune{digit}})
}

func TestClickAwayAndBackDropsTheFirstKeyOnAQuestion(t *testing.T) {
	a, clk, reply := twoSessions(t, questionRequestFor("red", "blue"))
	clickTab(t, a, 1)
	if a.Current().ID != "S2" {
		t.Fatalf("the tab click left %q current", a.Current().ID)
	}
	clk.advance(time.Second)
	clickTab(t, a, 0)
	if !grantsIn(afterReturn(t, a, clk, reply, '1')) {
		t.Fatal("a deliberate digit after the pause did not answer the question")
	}
}

func TestClickAwayAndBackDoesNotEnterTheReasonStageOnAnAllow(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	clickTab(t, a, 1)
	clk.advance(time.Second)
	clickTab(t, a, 0)
	afterReturn(t, a, clk, reply, '2')
	if a.Current().Approval().stage != stageReason {
		t.Fatal("a deliberate digit after the pause did not enter the reason stage")
	}
}

func TestNewSessionClickAndBackDropsTheFirstKey(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	if effects := clickNewSession(t, a); len(effects) != 1 {
		t.Fatalf("the new button produced %#v", effects)
	}
	// The host answers the effect by adding the session, which takes focus.
	_ = a.AddSession("S3", "session three")
	clk.advance(time.Second)
	clickTab(t, a, 0)
	afterReturn(t, a, clk, reply, '2')
}

func TestSessionCycleAndBackDropsTheFirstKey(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	a.cycleSession(1)
	clk.advance(time.Second)
	a.cycleSession(-1)
	if a.Current().ID != "S1" {
		t.Fatalf("cycling left %q current", a.Current().ID)
	}
	afterReturn(t, a, clk, reply, '2')
}

func TestDashboardRoundTripDropsTheFirstKey(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	a.toggleDashboard()
	clk.advance(time.Second)
	a.toggleDashboard()
	afterReturn(t, a, clk, reply, '2')
}

// A card raised for a session nobody is looking at is never painted, so its
// gate stays closed; it opens only from the moment the operator sees it.
func TestACardForABackgroundSessionIsNotArmedUntilPainted(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	bg := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S2", Request: blockedRequest(true), Reply: bg})
	card := a.views[1].Approval()
	drawAt(a, 100, 30) // S1 is on screen
	clk.advance(time.Hour)
	if card.painted || card.armed {
		t.Fatalf("a background card was painted=%v armed=%v", card.painted, card.armed)
	}
	clickTab(t, a, 1)
	// Arrived on screen: not yet painted, so a digit is dropped.
	a.Dispatch(ActionRune{Runes: []rune{'2'}})
	if _, ok := answered(bg); ok || card.stage != stageChoose || card.armed {
		t.Fatalf("a digit reached a card that was never painted: stage=%v armed=%v", card.stage, card.armed)
	}
	_ = reply
}
