package tui

import (
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// fakeClock is the app's clock for the activation-gate tests.
type fakeClock struct{ t time.Time }

func newFakeClock(a *App) *fakeClock {
	c := &fakeClock{t: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)}
	a.now = func() time.Time { return c.t }
	return c
}

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

// answered reports whether the blocked caller has been given a decision.
func answered(reply chan ui.Decision) (ui.Decision, bool) {
	select {
	case d := <-reply:
		return d, true
	default:
		return ui.Decision{}, false
	}
}

func grantsIn(effects []Effect) bool {
	for _, e := range effects {
		if dec, ok := e.(EffectDecide); ok && dec.Decision.Allow {
			return true
		}
	}
	return false
}

// An operator is typing a follow-up into the composer when the turn raises an
// approval. A bare digit selects "allow once", the rest of the typed text would
// become the grant's reason, and the Enter meant for the composer would record
// the grant. Nothing has been painted, so none of it may land.
func TestTypeAheadBeforeThePaintGrantsNothing(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "fix ")
	reply := raiseApprovalCold(a, true) // the card arrives mid-sentence
	typeText(a, "2 bugs in the parser")
	effects := key(a, "enter")

	if d, ok := answered(reply); ok {
		t.Fatalf("approval answered by type-ahead: Allow=%v By=%q Reason=%q (effects %v)", d.Allow, d.By, d.Reason, effects)
	}
	if grantsIn(effects) {
		t.Fatalf("grant issued from type-ahead: %+v", effects)
	}
	if card := a.Current().Approval(); card == nil || card.stage != stageChoose || card.reason.Value() != "" {
		t.Fatalf("type-ahead moved the card: %+v", card)
	}
}

// Same race, deny direction: a typed key must not refuse it with no chance to
// read it either.
func TestTypeAheadBeforeThePaintDeniesNothing(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "ad")
	reply := raiseApprovalCold(a, true)
	typeText(a, "d")
	key(a, "enter")
	if d, ok := answered(reply); ok {
		t.Fatalf("approval answered by a keystroke typed for the composer: Allow=%v Reason=%q", d.Allow, d.Reason)
	}
}

// The keys queued behind the request are dispatched after the first frame in
// the common case, so painting alone does not make the card ready: they must
// be dropped too.
func TestTypeAheadRightAfterThePaintIsDropped(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	reply := raiseApprovalCold(a, true)
	drawAt(a, 100, 30)

	clk.advance(10 * time.Millisecond)
	typeText(a, "2 bugs")
	clk.advance(10 * time.Millisecond)
	effects := key(a, "enter")
	if d, ok := answered(reply); ok || grantsIn(effects) || len(effects) != 0 {
		t.Fatalf("type-ahead after the paint reached the card: %+v %v", d, effects)
	}
	if card := a.Current().Approval(); card.stage != stageChoose || card.reason.Value() != "" {
		t.Fatalf("type-ahead moved the card: stage=%v reason=%q", card.stage, card.reason.Value())
	}
}

// A long burst of typing keeps the card closed however long it runs: every
// refused key restarts the quiet period. One key after a pause is a decision.
func TestABurstOfTypingStaysDroppedAndAPauseOpensTheCard(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	reply := raiseApprovalCold(a, true)
	drawAt(a, 100, 30)

	for i := 0; i < 30; i++ { // three seconds of keys 100 ms apart
		clk.advance(100 * time.Millisecond)
		typeText(a, "2")
	}
	if _, ok := answered(reply); ok || a.Current().Approval().stage != stageChoose {
		t.Fatal("a sustained burst reached the card")
	}

	clk.advance(approvalQuiet)
	if effects := a.Dispatch(ActionRune{Runes: []rune{'2'}}); len(effects) != 0 {
		t.Fatalf("selecting allow-once produced effects before a reason: %#v", effects)
	}
	if a.Current().Approval().stage != stageReason {
		t.Fatal("a deliberate key after a pause did not reach the card")
	}
	typeText(a, "reviewed the diff")
	effects := key(a, "enter")
	if len(effects) != 1 {
		t.Fatalf("enter produced %#v", effects)
	}
	dec, ok := effects[0].(EffectDecide)
	if !ok {
		t.Fatalf("enter produced %T, want EffectDecide", effects[0])
	}
	d := dec.Decision
	if !d.Allow || d.By != "human" || d.Reason != "reviewed the diff" {
		t.Fatalf("deliberate answer: %+v", d)
	}
}

// Enter typed ahead must not answer, even though the highlighted option is the
// safe one.
func TestEnterTypedAheadDoesNotAnswer(t *testing.T) {
	a, _ := newTestApp()
	reply := raiseApprovalCold(a, true)
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("enter before the paint produced %#v", effects)
	}
	drawAt(a, 100, 30)
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("enter at the paint produced %#v", effects)
	}
	if _, ok := answered(reply); ok {
		t.Fatal("the card was answered by typed-ahead enter")
	}
}

// Once the card has admitted a deliberate key it stays open: the operator is
// not made to wait again between choosing an option and typing the reason.
func TestAnAdmittedCardStaysOpen(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	raiseApprovalCold(a, true)
	drawAt(a, 100, 30)
	clk.advance(2 * approvalQuiet)
	key(a, "down")
	typeText(a, "x")
	key(a, "backspace")
	if !a.Current().Approval().armed {
		t.Fatal("the card did not latch open")
	}
}

// Esc only ever refuses, and quitting is not an answer, so neither waits.
func TestEscapeAndQuitAreNotGated(t *testing.T) {
	a, _ := newTestApp()
	reply := raiseApprovalCold(a, true)
	effects := key(a, "esc")
	if len(effects) != 1 {
		t.Fatalf("esc produced %#v", effects)
	}
	d, ok := effects[0].(EffectDecide)
	if !ok || d.Decision.Allow || d.Reply != reply {
		t.Fatalf("esc: %#v", effects[0])
	}

	a, _ = newTestApp()
	raiseApprovalCold(a, true)
	key(a, "ctrl+q")
	key(a, "ctrl+q")
	if !a.Quitting() {
		t.Fatal("quit keys did not reach the app while a card was closed")
	}
}

// Answering one card promotes the next, which has not been read either: a
// double press meant for the first card must not answer the second.
func TestTheNextQueuedCardIsGatedToo(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	raiseApprovalCold(a, true)
	second := raiseApprovalCold(a, true)
	drawAt(a, 100, 30)
	clk.advance(2 * approvalQuiet)
	typeText(a, "2")
	typeText(a, "ok")
	key(a, "enter")
	drawAt(a, 100, 30)
	clk.advance(20 * time.Millisecond)
	typeText(a, "2")
	typeText(a, "again")
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("a keypress meant for the first card answered the second: %#v", effects)
	}
	if _, ok := answered(second); ok {
		t.Fatal("the second card was answered by type-ahead")
	}
}

// A click is gated like a key: one landing inside the quiet period after the
// paint is dropped, one after it confirms.
func TestAClickInsideTheQuietPeriodIsDropped(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	reply := raiseApprovalCold(a, true)
	drawAt(a, 100, 30)
	card := a.Current().Approval()
	deny := card.optionRows[0]

	clk.advance(50 * time.Millisecond)
	if effects := a.Dispatch(ActionClick{X: deny.X + 3, Y: deny.Y, Button: 1}); len(effects) != 0 {
		t.Fatalf("a click inside the quiet period answered the card: %#v", effects)
	}
	clk.advance(approvalQuiet)
	effects := a.Dispatch(ActionClick{X: deny.X + 3, Y: deny.Y, Button: 1})
	if len(effects) != 1 {
		t.Fatalf("a click after the quiet period produced %#v", effects)
	}
	if d, ok := effects[0].(EffectDecide); !ok || d.Decision.Allow || d.Reply != reply {
		t.Fatalf("click: %#v", effects[0])
	}
}

func questionRequestFor(choices ...string) ui.Request {
	r := blockedRequest(true)
	r.Choices = choices
	return r
}

// The wheel is input like any other: while the card is inert a notch is
// dropped (it would move the highlight a later Enter confirms) and restarts the
// quiet period.
func TestWheelOnAnInertCardIsDroppedAndRestartsTheQuietPeriod(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	raiseApprovalCold(a, true)
	drawAt(a, 100, 30)
	card := a.Current().Approval()

	clk.advance(10 * time.Millisecond)
	a.Dispatch(ActionScroll{X: 5, Y: 5, Delta: 1})
	if card.option != 0 {
		t.Fatalf("a notch inside the quiet period moved the highlight to %d", card.option)
	}
	// A notch every 300 ms keeps restarting the period; Enter 30 ms after the
	// last one must not answer.
	for i := 0; i < 3; i++ {
		clk.advance(300 * time.Millisecond)
		a.Dispatch(ActionScroll{X: 5, Y: 5, Delta: 1})
	}
	clk.advance(30 * time.Millisecond)
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("enter 30 ms after a wheel notch answered the card: %#v", effects)
	}
	// After a real pause the wheel moves the highlight again.
	clk.advance(approvalQuiet)
	a.Dispatch(ActionScroll{X: 5, Y: 5, Delta: 1})
	if card.option != 1 {
		t.Fatalf("a notch after the pause left the highlight at %d, want 1", card.option)
	}
}

// A click that only moves the highlight is input too.
func TestAHighlightMovingClickOnAnInertCardIsDropped(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	raiseApprovalCold(a, true)
	drawAt(a, 100, 30)
	card := a.Current().Approval()
	allow := card.optionRows[1]

	clk.advance(20 * time.Millisecond)
	a.Dispatch(ActionClick{X: allow.X + 3, Y: allow.Y, Button: 1})
	if card.option != 0 {
		t.Fatalf("a click inside the quiet period moved the highlight to %d", card.option)
	}
	clk.advance(approvalQuiet)
	a.Dispatch(ActionClick{X: allow.X + 3, Y: allow.Y, Button: 1})
	if card.option != 1 {
		t.Fatalf("a click after the pause left the highlight at %d, want 1", card.option)
	}
}

// Only the left button confirms: a middle or right press on the highlighted row
// does nothing, on a permission card and on a question alike.
func TestOnlyTheLeftButtonOperatesTheCard(t *testing.T) {
	for _, button := range []int{0, 2, 3} {
		a, _ := newTestApp()
		clk := newFakeClock(a)
		reply := raiseApprovalCold(a, true)
		drawAt(a, 100, 30)
		clk.advance(2 * approvalQuiet)
		card := a.Current().Approval()
		deny := card.optionRows[0]
		if effects := a.Dispatch(ActionClick{X: deny.X + 3, Y: deny.Y, Button: button}); len(effects) != 0 {
			t.Fatalf("button %d answered the card: %#v", button, effects)
		}
		if _, ok := answered(reply); ok {
			t.Fatalf("button %d answered the blocked caller", button)
		}
	}

	a, _ := newTestApp()
	clk := newFakeClock(a)
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: questionRequestFor("red", "green"), Reply: reply})
	drawAt(a, 100, 30)
	clk.advance(2 * approvalQuiet)
	row := a.Current().Approval().optionRows[0]
	if effects := a.Dispatch(ActionClick{X: row.X + 3, Y: row.Y, Button: 2}); len(effects) != 0 {
		t.Fatalf("a middle click answered a question with its first choice: %#v", effects)
	}
	// The left button still answers.
	if effects := a.Dispatch(ActionClick{X: row.X + 3, Y: row.Y, Button: 1}); len(effects) != 1 {
		t.Fatalf("a left click did not answer: %#v", effects)
	}
}

// Time under an overlay is not time spent reading the card. Esc closes the
// palette, and an Enter 40 ms later must not answer a card nobody has looked at.
func TestAnOverlayOverACardRestartsTheQuietPeriod(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	key(a, "ctrl+p")
	if a.overlay == nil {
		t.Fatal("the palette did not open")
	}
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: questionRequestFor("red", "green"), Reply: reply})
	drawAt(a, 100, 30)
	clk.advance(3 * time.Second)

	key(a, "esc") // closes the palette
	if a.overlay != nil {
		t.Fatal("the palette did not close")
	}
	clk.advance(40 * time.Millisecond)
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("enter 40 ms after the palette closed answered the card: %#v", effects)
	}
	if _, ok := answered(reply); ok {
		t.Fatal("the card was answered from the keys that closed an overlay")
	}
	// And after a real pause it answers.
	clk.advance(approvalQuiet)
	if effects := key(a, "enter"); len(effects) != 1 {
		t.Fatalf("a deliberate enter after the pause produced %#v", effects)
	}
}

// Cancelling the turn only ever refuses, so it is not held back: Ctrl-C on a
// card that has not admitted input still cancels.
func TestCancelIsNotGated(t *testing.T) {
	a, _ := newTestApp()
	clk := newFakeClock(a)
	a.Current().SetBusy(true, "")
	raiseApprovalCold(a, true)
	drawAt(a, 100, 30)
	clk.advance(50 * time.Millisecond)
	effects := key(a, "ctrl+c")
	if len(effects) == 0 {
		t.Fatal("ctrl+c on a card inside its quiet period did nothing")
	}
	if _, ok := effects[0].(EffectCancel); !ok {
		t.Fatalf("ctrl+c produced %T, want EffectCancel", effects[0])
	}
}
