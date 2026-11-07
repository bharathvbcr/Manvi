package tui

import (
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// Adversarial tests for the approval card's input gate (ft-c86ae00210e7a8b0d6e35a3b457f208f,
// criterion 5). Each one tries to get a card to take input the operator did not
// deliberately give it. Time is the injectable fake clock; nothing sleeps.

// advThree is S1..S3 with S2 on screen, a fake clock, and a painted card on S2
// that has been left alone past the quiet period.
func advThree(t *testing.T, req ui.Request) (*App, *fakeClock, chan ui.Decision) {
	t.Helper()
	a, _ := newTestApp()
	_ = a.AddSession("S2", "session two")
	_ = a.AddSession("S3", "session three")
	clk := newFakeClock(a)
	drawAt(a, 100, 30)
	clickTab(t, a, 1)
	if a.Current().ID != "S2" {
		t.Fatalf("setup: current is %q", a.Current().ID)
	}
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S2", Request: req, Reply: reply})
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	return a, clk, reply
}

func advDecided(effects []Effect, reply chan ui.Decision) bool {
	if _, ok := answered(reply); ok {
		return true
	}
	for _, e := range effects {
		if _, ok := e.(EffectDecide); ok {
			return true
		}
	}
	return false
}

// ATTACK 1a: take the card off screen by shrinking the terminal. Draw refuses to
// paint a card it has no room for (and clears its click rect), but the card stays
// "painted" and keeps the keyboard, so a digit answers a card nobody can see.
func TestAdvResizeBelowCardHeightThenDigitAnswersAnInvisibleCard(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	card := a.Current().Approval()
	a.Dispatch(ActionResize{W: 100, H: 7})
	drawAt(a, 100, 7)
	if !card.cardRect.Empty() {
		t.Skipf("setup: the card is still drawn at 100x7 (rect %+v)", card.cardRect)
	}
	clk.advance(time.Second)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("a digit answered an approval card that is not on screen: %+v", effects)
	}
}

// ATTACK 1a': shrink, grow back, first key must be treated as a return.
func TestAdvResizeAwayAndBackDropsTheFirstKey(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	card := a.Current().Approval()
	a.Dispatch(ActionResize{W: 100, H: 7})
	drawAt(a, 100, 7)
	if !card.cardRect.Empty() {
		t.Skipf("setup: the card is still drawn at 100x7")
	}
	clk.advance(time.Second)
	a.Dispatch(ActionResize{W: 100, H: 30})
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("the first key after the card came back from a resize answered it: %+v", effects)
	}
}

// ATTACK 1b: RemoveSession of a session BELOW the active one. a.active keeps its
// index and now names a different view; the view that was on screen never goes
// through leave(), so its card stays painted and its gate stays open. Cycling back
// to it admits the very first key.
func TestAdvRemoveSessionBelowActiveLeavesTheOldCardOpen(t *testing.T) {
	a, clk, reply := advThree(t, blockedRequest(true))
	// S2's card is painted and quiet has passed; a stray wheel notch arms it.
	a.Dispatch(ActionScroll{X: 1, Y: 1, Delta: 1})
	a.RemoveSession("S1")
	if a.Current().ID != "S2" {
		t.Fatalf("setup: removing a lower session changed the view on screen to %q", a.Current().ID)
	}
	clk.advance(time.Second)
	a.cycleSession(1)
	clk.advance(time.Second)
	a.cycleSession(-1)
	if a.Current().ID != "S2" {
		t.Fatalf("setup: cycling reached %q", a.Current().ID)
	}
	drawAt(a, 100, 30)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("a card that was shown, then replaced by RemoveSession, answered the first key after return: %+v", effects)
	}
}

// Same hole without the wheel: the card is merely painted, quiet elapsed.
func TestAdvRemoveSessionBelowActiveUnarmedCardFirstKey(t *testing.T) {
	a, clk, reply := advThree(t, blockedRequest(true))
	a.RemoveSession("S1")
	clk.advance(time.Second)
	a.cycleSession(1)
	clk.advance(time.Second)
	a.cycleSession(-1)
	drawAt(a, 100, 30)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("returned card answered the first key: %+v", effects)
	}
}

// ATTACK 1c: an overlay over an armed card. The first ctrl+p after the quiet
// period is admitted and arms the card; from then on noteOverlayInput's refuse()
// does nothing (it only acts before the gate opens), so Esc then Enter lands on
// a card nobody read while the palette was up. This is the exact scenario
// noteOverlayInput's comment says it prevents.
func TestAdvPaletteOverAnArmedCardThenEscEnterAnswers(t *testing.T) {
	a, clk, reply := twoSessions(t, questionRequestFor("red", "blue"))
	key(a, "ctrl+p")
	if a.overlay == nil {
		t.Skip("setup: ctrl+p did not open the palette over the card")
	}
	clk.advance(2 * time.Second) // reading the palette
	typeText(a, "he")
	key(a, "esc")
	if a.overlay != nil {
		t.Fatal("setup: esc did not close the palette")
	}
	effects := key(a, "enter")
	if advDecided(effects, reply) {
		t.Fatalf("Esc then Enter answered a card that was behind a palette: %+v", effects)
	}
}

// ATTACK 1d: the card arrives while a palette is open. Every overlay input is
// counted against it, including the Esc that closes it, so Enter right after
// must be dropped.
func TestAdvCardArrivingUnderAPaletteIsNotAnsweredByEscEnter(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	drawAt(a, 100, 30)
	key(a, "ctrl+p")
	if a.overlay == nil {
		t.Fatal("setup: no palette")
	}
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: reply})
	drawAt(a, 100, 30)
	clk.advance(2 * time.Second)
	typeText(a, "he")
	key(a, "esc")
	effects := key(a, "enter")
	if advDecided(effects, reply) {
		t.Fatalf("Enter answered a card first shown behind a palette: %+v", effects)
	}
}

// ATTACK 2a: a stray wheel notch. A notch is admitted once the quiet period has
// passed and latches armed, so the next digit, a millisecond later, answers. The
// mouse grammar comments name the stray trackpad brush as the thing to guard.
func TestAdvStrayWheelNotchThenDigitDoesNotAnswer(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	a.Dispatch(ActionScroll{X: 1, Y: 1, Delta: 1})
	clk.advance(time.Millisecond)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("a wheel notch armed the card and the next digit answered: %+v", effects)
	}
}

// ATTACK 2b: a wheel notch right after returning consumes the returned-drop.
// The digit after a quiet period is then admitted although no digit was ever the
// "first input".
func TestAdvWheelAfterReturnDoesNotConsumeTheReturnedDrop(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	clickTab(t, a, 1)
	clk.advance(time.Second)
	clickTab(t, a, 0)
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	a.Dispatch(ActionScroll{X: 1, Y: 1, Delta: 1})
	clk.advance(approvalQuiet + time.Millisecond)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("a wheel notch used up the returned-drop and the digit answered: %+v", effects)
	}
}

// ATTACK 2c: mouse release and motion are not input to the card.
func TestAdvMotionAndReleaseDoNotConsumeTheReturnedDrop(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	clickTab(t, a, 1)
	clk.advance(time.Second)
	clickTab(t, a, 0)
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	a.Dispatch(ActionMotion{X: 5, Y: 5})
	a.Dispatch(ActionRelease{})
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) || a.Current().Approval().stage != stageChoose {
		t.Fatalf("motion/release let the first key act: %+v", effects)
	}
}

// ATTACK 2d: a click that lands exactly on a returned card's option is the first
// input and is dropped; a double click inside the quiet period never answers.
func TestAdvClickOnReturnedCardOptionIsDroppedAndDoubleClickDoesNotAnswer(t *testing.T) {
	a, clk, reply := twoSessions(t, questionRequestFor("red", "blue"))
	clickTab(t, a, 1)
	clk.advance(time.Second)
	clickTab(t, a, 0)
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	card := a.Current().Approval()
	if len(card.optionRows) == 0 {
		t.Fatal("setup: no option rows")
	}
	row := card.optionRows[0]
	click := ActionClick{X: row.X + 1, Y: row.Y, Button: 1}
	for i := 0; i < 3; i++ {
		effects := a.Dispatch(click)
		clk.advance(50 * time.Millisecond)
		if advDecided(effects, reply) {
			t.Fatalf("click %d on a returned card inside quiet answered it: %+v", i, effects)
		}
	}
}

// ATTACK 3a: a paste is one input. After return it is the dropped first input.
func TestAdvPasteOfDigitsAfterReturnIsDroppedAndNeverAnswers(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	clickTab(t, a, 1)
	clk.advance(time.Second)
	clickTab(t, a, 0)
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	effects := a.Dispatch(ActionPaste{Text: "2\n"})
	if advDecided(effects, reply) || a.Current().Approval().stage != stageChoose {
		t.Fatalf("a paste after return acted on the card: %+v", effects)
	}
	// The paste consumed the drop and restarted quiet; a digit now is dropped.
	effects = a.Dispatch(ActionRune{Runes: []rune{'2'}})
	if advDecided(effects, reply) || a.Current().Approval().stage != stageChoose {
		t.Fatalf("a digit right after the paste acted: %+v", effects)
	}
}

// ATTACK 3b: a paste into an armed card at the choose stage must not answer or
// change stage, whatever it contains.
func TestAdvPasteIntoArmedCardAtChooseStageDoesNothing(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	a.Dispatch(ActionKey{Binding: "down"}) // arms
	clk.advance(time.Millisecond)
	effects := a.Dispatch(ActionPaste{Text: "2\n1\n2\n"})
	if advDecided(effects, reply) || a.Current().Approval().stage != stageChoose {
		t.Fatalf("a paste acted on an armed card at the choose stage: %+v stage=%v", effects, a.Current().Approval().stage)
	}
	// A multi-rune "digits" chunk is not a digit binding.
	effects = a.Dispatch(ActionRune{Runes: []rune{'1', '2'}})
	if advDecided(effects, reply) || a.Current().Approval().stage != stageChoose {
		t.Fatalf("a multi-rune chunk acted: %+v", effects)
	}
}

// ATTACK 3c: escape and cancel are always live and must not be able to consume
// the returned-drop (Esc from the reason stage steps back without answering).
func TestAdvEscBeforeFirstRealKeyDoesNotConsumeTheReturnedDrop(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	card := a.Current().Approval()
	a.Dispatch(ActionRune{Runes: []rune{'2'}}) // arms and enters the reason stage
	if card.stage != stageReason {
		t.Fatalf("setup: stage %v", card.stage)
	}
	clickTab(t, a, 1)
	clk.advance(time.Second)
	clickTab(t, a, 0)
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	a.Dispatch(ActionKey{Binding: "esc"}) // back to choose, no decision
	if card.stage != stageChoose {
		t.Fatalf("setup: esc left stage %v", card.stage)
	}
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("after Esc the first digit answered: %+v", effects)
	}
}

// ATTACK 4: leave is idempotent, never invents a return for an unseen card, and
// every return restarts the period.
func TestAdvLeaveWhileUnpaintedLeavesReturnedFalse(t *testing.T) {
	c := NewApprovalCard(blockedRequest(true), make(chan ui.Decision, 1))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.clock = func() time.Time { return now }
	c.leave()
	c.leave()
	if c.returned {
		t.Fatal("leave before any paint marked the card returned")
	}
	c.markPainted()
	now = now.Add(approvalQuiet + time.Millisecond)
	if !c.admit() {
		t.Fatal("the first input after the first-paint quiet was dropped: returned leaked")
	}
}

func TestAdvRepeatedLeaveAndReturnKeepsDroppingFirstKey(t *testing.T) {
	c := NewApprovalCard(blockedRequest(true), make(chan ui.Decision, 1))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	c.clock = func() time.Time { return now }
	c.markPainted()
	now = now.Add(time.Second)
	for i := 0; i < 3; i++ {
		c.leave()
		c.leave()
		now = now.Add(time.Second)
		c.markPainted()
		now = now.Add(approvalQuiet + time.Millisecond)
		if c.admit() {
			t.Fatalf("round %d: first input after return admitted", i)
		}
		now = now.Add(approvalQuiet - time.Millisecond)
		if c.admit() {
			t.Fatalf("round %d: input inside the restarted quiet admitted", i)
		}
		now = now.Add(approvalQuiet + time.Millisecond)
		if !c.admit() {
			t.Fatalf("round %d: deliberate input after quiet dropped", i)
		}
	}
}

// ATTACK 1e: a second approval for the same session must not inherit the first
// card's gate.
func TestAdvNextQueuedCardDoesNotInheritArmed(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	second := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: second})
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}}) // deny the first
	if !advDecided(effects, reply) {
		t.Fatal("setup: the first card was not answered")
	}
	next := a.Current().Approval()
	if next == nil || next.Reply != second {
		t.Fatalf("setup: next card is %+v", next)
	}
	// Same frame, then a double-tapped digit.
	clk.advance(time.Millisecond)
	drawAt(a, 100, 30)
	clk.advance(time.Millisecond)
	effects = a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, second) {
		t.Fatalf("the double-tapped digit answered the second card: %+v", effects)
	}
}

// ATTACK 1f: a card that arrives while the operator is on the dashboard.
func TestAdvCardArrivingOnTheDashboardIsFirstPaintGated(t *testing.T) {
	a, clk, _ := twoSessions(t, blockedRequest(true))
	a.toggleDashboard()
	second := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S2", Request: blockedRequest(true), Reply: second})
	clk.advance(time.Hour)
	a.showSession(1)
	drawAt(a, 100, 30)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, second) {
		t.Fatalf("a card first shown on opening a session answered the first key: %+v", effects)
	}
}

// ATTACK 1g: the sessions overlay (ctrl+... switcher) re-selecting the current
// session is a leave and return.
func TestAdvSessionsOverlayReselectDropsTheFirstKey(t *testing.T) {
	a, clk, reply := twoSessions(t, blockedRequest(true))
	a.overlay = SessionsOverlay(a.views, a.active)
	a.overlay.MoveTo(a.active)
	clk.advance(time.Second)
	a.acceptOverlay()
	drawAt(a, 100, 30)
	clk.advance(approvalQuiet + time.Millisecond)
	effects := a.Dispatch(ActionRune{Runes: []rune{'1'}})
	if advDecided(effects, reply) {
		t.Fatalf("re-selecting the session through the switcher kept the gate open: %+v", effects)
	}
}

// Removing a session below the one on screen must leave the same view on
// screen: the index names a view, and the splice moves the view, not the index.
func TestRemoveSessionBelowActiveKeepsTheSameViewOnScreen(t *testing.T) {
	a, _, _ := advThree(t, blockedRequest(true))
	before := a.Current()
	a.RemoveSession("S1")
	if a.Current() != before {
		t.Fatalf("the view on screen changed from %q to %q", before.ID, a.Current().ID)
	}
	a.RemoveSession("S3") // above the active one: nothing moves
	if a.Current() != before {
		t.Fatalf("removing a higher session changed the view on screen to %q", a.Current().ID)
	}
}
