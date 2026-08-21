package tui

import (
	"strings"
	"testing"

	"manvi/ui"
	"manvi/ui/render"
)

// The mouse grammar, uniform across surfaces: a click moves the highlight, a
// click on the highlighted thing confirms it, and a click outside a floating
// surface dismisses it. These tests exist because every one of those paths
// was once either absent — clicks on an open overlay were discarded whole —
// or one stale-geometry bug away from acting on the wrong row.

// drawAt renders the app so its hit-test geometry is recorded, the same
// arrangement the runner uses every frame.
func drawAt(a *App, w, h int) *render.Buffer {
	b := render.NewBuffer(w, h)
	a.Draw(b)
	return b
}

func TestPaletteAnswersClicks(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	if a.overlay == nil || a.overlay.Kind != OverlayPalette {
		t.Fatalf("ctrl+p opened %+v", a.overlay)
	}
	drawAt(a, 100, 30)

	// The rows are the host's commands in order; click the second.
	row := a.overlay.listRect.Y + 1
	a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: row, Button: 1})
	if a.overlay == nil {
		t.Fatal("the first click accepted rather than moving the highlight")
	}
	if got := a.overlay.Sel(); got != 1 {
		t.Fatalf("the click landed on row 1 but the highlight is on %d", got)
	}

	// Second click on the highlighted row accepts it. /check takes a PATH, so
	// accepting arms the composer rather than running it with no argument.
	effects := a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: row, Button: 1})
	if a.overlay != nil {
		t.Fatal("the confirming click left the palette open")
	}
	if len(effects) != 0 {
		t.Fatalf("accepting ran a command that needs an argument: %#v", effects)
	}
	if got := a.Current().Prompt.Value(); got != "/check " {
		t.Fatalf("prompt = %q, want /check waiting for its argument", got)
	}

	// A command with no arguments runs from the same two clicks.
	a.Current().Prompt.Clear()
	key(a, "ctrl+p")
	drawAt(a, 100, 30)
	// One click, not two: row 0 is already the highlighted row, and a click on
	// the row the highlight is already on is the accepting one.
	first := a.overlay.listRect.Y
	effects = a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: first, Button: 1})
	if len(effects) != 1 {
		t.Fatalf("accepting /doctor produced %#v", effects)
	}
	if cmd, ok := effects[0].(EffectCommand); !ok || cmd.Name != "doctor" {
		t.Fatalf("accepted %#v, want the /doctor command", effects[0])
	}
}

func TestOverlayClickAwayDismissesAndAMissDoesNot(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	drawAt(a, 100, 30)

	// On the border, one cell outside the item rows: swallowed, not a
	// dismissal — a near-miss must not close the list.
	a.Dispatch(ActionClick{X: a.overlayRect.X, Y: a.overlayRect.Y, Button: 1})
	if a.overlay == nil {
		t.Fatal("a click on the frame dismissed the palette")
	}

	// Well outside it: dismissed, as Esc does.
	a.Dispatch(ActionClick{X: 0, Y: 0, Button: 1})
	if a.overlay != nil {
		t.Fatal("a click away from the palette did not dismiss it")
	}
}

func TestOverlayHoverFollowsThePointer(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	drawAt(a, 100, 30)

	y := a.overlay.listRect.Y + 2
	if !a.overlay.HoverAt(a.overlay.listRect.X+4, y) {
		t.Fatal("the first hover reported no change")
	}
	if a.overlay.HoverAt(a.overlay.listRect.X+6, y) {
		t.Fatal("a second hover on the same row reported a change — that is a repaint per motion event")
	}
	// The hover is not the selection: the keyboard's Enter still lands on the
	// row the keys chose.
	if a.overlay.Sel() == 2 {
		t.Fatal("a hover moved the selection")
	}
}

func TestTabStripClickSwitchesSessions(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	drawAt(a, 100, 30)
	if len(a.tabs) < 3 { // two tabs and the new button
		t.Fatalf("the strip recorded %d refs", len(a.tabs))
	}

	// Click the first session's tab.
	var tabRect render.Rect
	for _, tab := range a.tabs {
		if !tab.IsNewButton && tab.SessionIndex == 0 {
			tabRect = tab.Rect
		}
	}
	if tabRect.Empty() {
		t.Fatal("no tab was recorded for session 0")
	}
	a.Dispatch(ActionClick{X: tabRect.X + 1, Y: tabRect.Y, Button: 1})
	if a.active != 0 {
		t.Fatalf("clicking the first tab left session %d active", a.active)
	}
}

func TestTabStripNewButtonAsksForASession(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	drawAt(a, 100, 30)

	for _, tab := range a.tabs {
		if !tab.IsNewButton {
			continue
		}
		effects := a.Dispatch(ActionClick{X: tab.Rect.X + 1, Y: tab.Rect.Y, Button: 1})
		if len(effects) != 1 {
			t.Fatalf("the new button produced %#v", effects)
		}
		if _, ok := effects[0].(EffectNewSession); !ok {
			t.Fatalf("the new button produced %#v", effects[0])
		}
		return
	}
	t.Fatal("no new-session button was recorded")
}

func TestTheStripIsNotDrawnForASingleSession(t *testing.T) {
	a, _ := newTestApp()
	drawAt(a, 100, 30)
	if len(a.tabs) != 0 {
		t.Fatal("a one-session UI paid a row for a strip that says nothing")
	}
}

func TestApprovalCardAnswersClicks(t *testing.T) {
	a, _ := newTestApp()
	reply := raiseApproval(a, true)
	drawAt(a, 100, 30)
	card := a.Current().Approval()
	if len(card.optionRows) != 2 {
		t.Fatalf("the card recorded %d option rows", len(card.optionRows))
	}

	// Deny is the highlighted row; clicking it confirms immediately.
	deny := card.optionRows[0]
	effects := a.Dispatch(ActionClick{X: deny.X + 3, Y: deny.Y, Button: 1})
	if len(effects) != 1 {
		t.Fatalf("the confirming click produced %#v", effects)
	}
	d, ok := effects[0].(EffectDecide)
	if !ok || d.Decision.Allow {
		t.Fatalf("deny produced %#v", effects[0])
	}
	select {
	case got := <-reply:
		if got.Allow {
			t.Fatal("the blocked caller was told allow")
		}
	default:
		t.Fatal("the decision never reached the blocked caller")
	}
}

func TestApprovalClickMovesBeforeItConfirms(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	drawAt(a, 100, 30)
	card := a.Current().Approval()
	allow := card.optionRows[1]

	// First click on the unhighlighted row: moves, decides nothing.
	if effects := a.Dispatch(ActionClick{X: allow.X + 3, Y: allow.Y, Button: 1}); len(effects) != 0 {
		t.Fatalf("the first click decided: %#v", effects)
	}
	if card.option != 1 {
		t.Fatalf("the highlight did not move to allow: %d", card.option)
	}

	// Second click: to the reason stage, still no decision.
	if effects := a.Dispatch(ActionClick{X: allow.X + 3, Y: allow.Y, Button: 1}); len(effects) != 0 {
		t.Fatalf("choosing allow decided before the reason: %#v", effects)
	}
	if card.Prompt() == nil {
		t.Fatal("the second click did not reach the reason stage")
	}
	typeText(a, "reviewed against the plan")

	// The chosen row is the confirm button from the reason stage.
	if effects := a.Dispatch(ActionClick{X: allow.X + 3, Y: allow.Y, Button: 1}); len(effects) != 1 {
		t.Fatalf("the reason-stage confirm produced %#v", effects)
	} else if d := effects[0].(EffectDecide); !d.Decision.Allow || d.Decision.Reason == "" {
		t.Fatalf("the grant went out without its reason: %+v", d.Decision)
	}
}

func TestClicksDoNotReachBehindAnApprovalCard(t *testing.T) {
	a, _ := newTestApp()
	for i := 0; i < 4; i++ {
		a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{Kind: ui.KindNotice, Text: "entry"}})
	}
	raiseApproval(a, true)
	drawAt(a, 100, 30)

	// A click on the transcript region, far above the card, selects nothing.
	a.Dispatch(ActionClick{X: 5, Y: 2, Button: 1})
	if a.Current().Scroll.Selected() != nil {
		t.Fatal("a click reached the transcript behind the modal card")
	}
	if a.Current().Approval() == nil {
		t.Fatal("the card vanished under a stray click")
	}
}

func TestDashboardSecondClickOpensTheSession(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	key(a, "ctrl+g") // enters the dashboard with the highlight on the active session
	if a.mode != ModeDashboard {
		t.Fatal("ctrl+g did not open the dashboard")
	}
	drawAt(a, 100, 30)

	// The highlight starts on the active session (S2, index 1); the row the
	// pointer is NOT on is session 0.
	full := render.Rect{X: 0, Y: 0, W: 100, H: 30}
	y := -1
	for row := 0; row < 30; row++ {
		if idx := a.dashboard.HitTest(full, row, len(a.views)); idx == 0 {
			y = row
			break
		}
	}
	if y < 0 {
		t.Fatal("the dashboard hit test found no row for session 0")
	}

	a.Dispatch(ActionClick{X: 10, Y: y, Button: 1})
	if a.dashboard.Selected() != 0 {
		t.Fatalf("the click selected %d", a.dashboard.Selected())
	}
	if a.mode != ModeDashboard {
		t.Fatal("the first click opened the session rather than selecting it")
	}
	a.Dispatch(ActionClick{X: 10, Y: y, Button: 1})
	if a.mode != ModeSession || a.active != 0 {
		t.Fatalf("the second click left mode=%v active=%d", a.mode, a.active)
	}
}

func TestPromptClickPositionsTheCaret(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "hello world")
	drawAt(a, 100, 30)
	v := a.Current()
	if v.promptRect.Empty() {
		t.Fatal("the composer recorded no geometry")
	}

	a.Dispatch(ActionClick{X: v.promptRect.X + 4, Y: v.promptRect.Y, Button: 1})
	if got := v.Prompt.cursor; got != 4 {
		t.Fatalf("the caret landed at %d, want 4", got)
	}
	if v.Focus != CtxPrompt {
		t.Fatal("clicking the composer did not focus it")
	}

	// A click past the line's end lands at the end, not on the next row.
	a.Dispatch(ActionClick{X: v.promptRect.Right() - 1, Y: v.promptRect.Y, Button: 1})
	if got := v.Prompt.cursor; got != len("hello world") {
		t.Fatalf("a click past the end left the caret at %d", got)
	}
}

func TestRightClickCopiesTheEntryUnderIt(t *testing.T) {
	a, _ := newTestApp()
	for i := 0; i < 5; i++ {
		a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindNotice, Text: "entry number " + itoa(i),
		}})
	}
	drawAt(a, 80, 20)

	sb := a.Current().Scroll
	row := sb.view.Bottom() - 1 // the newest entry, bottom-aligned
	effects := a.Dispatch(ActionClick{X: 4, Y: row, Button: 3})
	if len(effects) != 1 {
		t.Fatalf("right-click produced %#v", effects)
	}
	cp, ok := effects[0].(EffectCopy)
	if !ok || !strings.Contains(cp.Text, "entry number 4") {
		t.Fatalf("right-click copied %q", cp.Text)
	}
}

func TestDoubleClickFoldsTheEntry(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 3)
	drawAt(a, 80, 20)

	sb := a.Current().Scroll
	row := sb.view.Bottom() - 1
	a.Dispatch(ActionClick{X: 4, Y: row, Button: 1})
	sel := sb.Selected()
	if sel == nil || !sel.Foldable {
		t.Fatal("the fixture's newest entry is not foldable")
	}
	a.Dispatch(ActionClick{X: 4, Y: row, Button: 1})
	if !sel.Folded {
		t.Fatal("the double click did not fold the entry")
	}
}

func TestScrollbarDragScrollsTheTranscript(t *testing.T) {
	a, _ := newTestApp()
	for i := 0; i < 200; i++ {
		a.Dispatch(ActionEvent{SessionID: "S1", Event: ui.Event{
			Kind: ui.KindNotice, Text: "row " + itoa(i),
		}})
	}
	drawAt(a, 80, 20)

	sb := a.Current().Scroll
	bar := sb.ScrollbarCol()
	if bar < 0 {
		t.Fatal("a 200-row transcript in 20 rows drew no scrollbar")
	}

	// Grab the thumb and drag it to the top of the track.
	a.Dispatch(ActionClick{X: bar, Y: sb.view.Bottom() - 1, Button: 1})
	if !a.dragBar {
		t.Fatal("pressing the scrollbar did not grab it")
	}
	a.Dispatch(ActionMotion{X: bar, Y: sb.view.Y, Button: 1})
	if sb.Following() {
		t.Fatal("dragging to the top left the viewport pinned to the newest row")
	}
	a.Dispatch(ActionRelease{X: bar, Y: sb.view.Y, Button: 1})
	if a.dragBar {
		t.Fatal("the release did not end the drag")
	}
}

func TestWheelOverAnOpenOverlayMovesItsSelection(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+p")
	drawAt(a, 100, 30)

	a.Dispatch(ActionScroll{X: a.overlayRect.X + 1, Y: a.overlayRect.Y + 2, Delta: 3})
	if a.overlay.Sel() != 1 {
		t.Fatalf("the wheel moved the palette to %d", a.overlay.Sel())
	}
}

// framesDiffer reports whether two drawn frames differ in any cell.
func framesDiffer(a, b *render.Buffer) bool {
	if a.W != b.W || a.H != b.H {
		return true
	}
	for y := 0; y < a.H; y++ {
		for x := 0; x < a.W; x++ {
			ca, cb := a.Cell(x, y), b.Cell(x, y)
			if ca.R != cb.R || !ca.Style.Equal(cb.Style) {
				return true
			}
		}
	}
	return false
}

func TestTheEmptyPanesRainMovesWithTheTick(t *testing.T) {
	a, _ := newTestApp()
	a.RemoveSession("S1")
	before := drawAt(a, 80, 24)
	a.Dispatch(ActionTick{})
	after := drawAt(a, 80, 24)
	if !framesDiffer(before, after) {
		t.Fatal("the idle backdrop did not move between ticks")
	}
}

func TestAnimationIsAbsentUnderThePlainTheme(t *testing.T) {
	a, _ := newTestApp()
	a.RemoveSession("S1")
	a.Theme = NoColorTheme()
	for i := 0; i < 30; i++ {
		a.Dispatch(ActionTick{})
	}
	before := drawAt(a, 80, 24)
	a.Dispatch(ActionTick{})
	after := drawAt(a, 80, 24)
	if framesDiffer(before, after) {
		t.Fatal("the plain theme chose no animation, but the frame moved between ticks")
	}
}
