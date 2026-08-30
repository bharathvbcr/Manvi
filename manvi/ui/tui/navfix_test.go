package tui

// Regression tests for the navigation repair round: every test below names a
// behaviour that was broken and is now load-bearing. The defects shared one
// shape — a surface that worked for the keyboard but not the pointer, or a
// surface that answered a pointer with an authority the keyboard never grants
// — and each test pins the repair at the seam where the defect lived.

import (
	"strings"
	"testing"

	"manvi/ui/render"
)

// TestTheOverlayWheelWorksWithoutASession. The scroll handler used to check
// for a current session before it checked for an overlay, so on the empty
// startup screen — the screen the palette exists to guide the operator off —
// the wheel did nothing. The check order is now the surface order: the
// topmost thing takes the wheel first.
func TestTheOverlayWheelWorksWithoutASession(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 30})
	a.openPalette()
	drawAt(a, 100, 30)

	a.Dispatch(ActionScroll{X: a.overlayRect.X + 1, Y: a.overlayRect.Y + 2, Delta: 3})
	if a.overlay.Sel() != 1 {
		t.Fatalf("with no session at all, the wheel left the palette at %d", a.overlay.Sel())
	}
	a.Dispatch(ActionScroll{X: a.overlayRect.X + 1, Y: a.overlayRect.Y + 2, Delta: -3})
	if a.overlay.Sel() != 0 {
		t.Fatalf("the wheel back left the palette at %d", a.overlay.Sel())
	}
}

// TestANonLeftClickConfirmsNothing. The two-step grammar — first click moves
// the highlight, second click on the highlighted row confirms — existed so a
// stray pointer press cannot act. The overlay branch never looked at the
// button, so a right-click on the highlighted palette row ran the command on
// it. This presses every other button on the confirming row and asserts that
// nothing happened.
func TestANonLeftClickConfirmsNothing(t *testing.T) {
	for _, button := range []int{2, 3} {
		a, _ := newTestApp()
		a.openPalette()
		drawAt(a, 100, 30)

		// Row 0 is already highlighted, so one press there is the
		// confirming press — for the left button. For any other it must be
		// inert: no accept, no highlight move, no dismissal.
		effects := a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: a.overlay.listRect.Y, Button: button})
		if len(effects) != 0 {
			t.Fatalf("button %d on the highlighted row produced %#v", button, effects)
		}
		if a.overlay == nil {
			t.Fatalf("button %d dismissed the palette", button)
		}
		if a.overlay.Sel() != 0 {
			t.Fatalf("button %d moved the highlight to %d", button, a.overlay.Sel())
		}
		if !a.Current().Prompt.Empty() {
			t.Fatalf("button %d armed the composer with %q", button, a.Current().Prompt.Value())
		}
	}
}

// TestTheThemePickerIgnoresANonLeftClick is the same guarantee at the surface
// where a wrong confirm is visible to the whole terminal.
func TestTheThemePickerIgnoresANonLeftClick(t *testing.T) {
	a, _ := newTestApp()
	a.runCommand("theme", "")
	drawAt(a, 100, 30)
	row := a.overlay.listRect.Y + 1 // light
	a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: row, Button: 1})
	effects := a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: row, Button: 3})
	if len(effects) != 0 || a.Theme.Name != "dark" {
		t.Fatalf("a right-click confirm switched the theme to %q (%#v)", a.Theme.Name, effects)
	}
	// The first click still counts: the left button confirms from here.
	a.Dispatch(ActionClick{X: a.overlay.listRect.X + 2, Y: row, Button: 1})
	if a.Theme.Name != "light" {
		t.Fatalf("the left-click confirm left the theme at %q", a.Theme.Name)
	}
}

// TestTheApprovalCardIsModalForTheWheel. The card is modal for keys and for
// clicks — an approval answered by accident is indistinguishable from one
// answered on purpose — but the wheel went straight to the transcript behind
// it. Now a notch moves the option highlight and nothing behind moves.
func TestTheApprovalCardIsModalForTheWheel(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 40)
	drawAt(a, 100, 30)
	raiseApproval(a, true)
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("the approval request raised no card")
	}
	at := card.option
	followed := a.Current().Scroll.Following()

	a.Dispatch(ActionScroll{X: 50, Y: 10, Delta: 3})
	if card.option == at {
		t.Fatal("the wheel did not reach the card's options")
	}
	if a.Current().Scroll.Following() != followed {
		t.Fatal("the wheel scrolled the transcript behind the modal card")
	}
	a.Dispatch(ActionScroll{X: 50, Y: 10, Delta: -3})
	if card.option != at {
		t.Fatalf("scrolling back left the option at %d, want %d", card.option, at)
	}
}

// TestTheDashboardDrawsOnlyTheWindowAroundTheSelection. The dashboard exists
// for fan-out, and it used to draw the first rows that fit and stop: the
// highlight walked off the bottom, Enter opened a session nobody could see,
// and the overflow line answered clicks for a session that had never been
// drawn. The window now follows the selection, the way the overlays' does.
func TestTheDashboardDrawsOnlyTheWindowAroundTheSelection(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 12})
	for i := 0; i < 12; i++ {
		a.AddSession("S"+itoa(i), "session "+itoa(i))
	}
	key(a, "ctrl+g")
	if a.mode != ModeDashboard {
		t.Fatal("ctrl+g did not open the dashboard")
	}

	// Walk to the last session, then ask the frame where it is.
	a.dashboard.Select(len(a.views) - 1)
	frame := strings.Join(screen(a, 100, 12), "\n")
	last := a.views[len(a.views)-1]
	if !strings.Contains(frame, last.Title) {
		t.Fatalf("the selected session is off-screen:\n%s", frame)
	}
	if !strings.Contains(frame, "above") {
		t.Fatalf("a windowed list showed no overflow-above cue:\n%s", frame)
	}

	// Enter opens the session the operator can actually see.
	key(a, "enter")
	if a.mode != ModeSession || a.Current() != last {
		t.Fatalf("enter opened %v, want the session the highlight was on", a.mode)
	}
}

// TestTheDashboardHitTestReadsTheFrameItDrew. The old hit test recomputed the
// layout from padding constants, and at its edge it mapped the "… N more"
// overflow line onto a session that was never drawn — a click there selected
// that session sight unseen. This uses a dashboard too short for its sessions
// and clicks the overflow line: nothing may answer.
func TestTheDashboardHitTestReadsTheFrameItDrew(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 12})
	for i := 0; i < 12; i++ {
		a.AddSession("S"+itoa(i), "session "+itoa(i))
	}
	key(a, "ctrl+g")
	a.dashboard.Select(0)
	frame := strings.Join(screen(a, 100, 12), "\n")

	// Find the overflow line and click it.
	y := -1
	for row, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "more") {
			y = row
			break
		}
	}
	if y < 0 {
		t.Fatalf("a 12-session dashboard in 12 rows showed no overflow line:\n%s", frame)
	}
	a.Dispatch(ActionClick{X: 10, Y: y, Button: 1})
	if a.dashboard.Selected() != 0 {
		t.Fatalf("a click on the overflow line selected %d — a session that was never drawn",
			a.dashboard.Selected())
	}

	// And a click on a drawn row still selects it.
	if len(a.dashboard.rows) < 2 {
		t.Fatal("the frame recorded no rows")
	}
	row := a.dashboard.rows[1]
	a.Dispatch(ActionClick{X: row.X + 1, Y: row.Y, Button: 1})
	if a.dashboard.Selected() != 1 {
		t.Fatalf("a click on the second drawn row selected %d", a.dashboard.Selected())
	}
}

// TestTheDashboardMovesInPagesAndToItsEnds.
func TestTheDashboardMovesInPagesAndToItsEnds(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 14})
	for i := 0; i < 20; i++ {
		a.AddSession("S"+itoa(i), "session "+itoa(i))
	}
	key(a, "ctrl+g")
	a.dashboard.Select(0)
	drawAt(a, 100, 14)

	key(a, "pgdn")
	if got := a.dashboard.Selected(); got == 0 {
		t.Fatal("pgdn did not move the dashboard highlight")
	}
	key(a, "G")
	if got := a.dashboard.Selected(); got != len(a.views)-1 {
		t.Fatalf("G put the highlight at %d, want the last session", got)
	}
	key(a, "g")
	if got := a.dashboard.Selected(); got != 0 {
		t.Fatalf("g put the highlight at %d, want the first", got)
	}
	key(a, "home")
	if got := a.dashboard.Selected(); got != 0 {
		t.Fatalf("home moved the highlight to %d", got)
	}
	key(a, "end")
	if got := a.dashboard.Selected(); got != len(a.views)-1 {
		t.Fatalf("end put the highlight at %d, want the last session", got)
	}
	// The horizontal arrows steer the list too: the dashboard is a list,
	// and an unbound arrow on it reads as a dead key.
	key(a, "left")
	if got := a.dashboard.Selected(); got != len(a.views)-2 {
		t.Fatalf("left moved to %d, want one up", got)
	}
	key(a, "l")
	if got := a.dashboard.Selected(); got != len(a.views)-1 {
		t.Fatalf("l moved to %d, want one down", got)
	}
}

// TestOverlaysPageByTheWindowTheyDrew.
func TestOverlaysPageByTheWindowTheyDrew(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 30})
	for i := 0; i < 40; i++ {
		a.AddSession("S"+itoa(i), "session "+itoa(i))
	}
	a.overlay = SessionsOverlay(a.views, a.active)
	drawAt(a, 100, 30)

	key(a, "pgdn")
	if a.overlay.Sel() == 0 {
		t.Fatal("pgdn did not page the session picker")
	}
	before := a.overlay.Sel()
	key(a, "pgup")
	if a.overlay.Sel() >= before {
		t.Fatalf("pgup left the picker at %d after %d", a.overlay.Sel(), before)
	}
	if a.overlay.Sel() != 0 {
		t.Fatalf("pgup from the first page left the picker at %d", a.overlay.Sel())
	}
}

// TestShiftTabCyclesBackThroughTheSessions pins the binding the session strip
// never had: CmdPrevSession was dispatched from the day it was written and
// bound to no key.
func TestShiftTabCyclesBackThroughTheSessions(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	a.AddSession("S3", "session three")
	first := a.Current()

	key(a, "shift+tab")
	if a.Current() == first {
		t.Fatal("shift+tab did not move to a previous session")
	}
	key(a, "ctrl+t")
	if a.Current() != first {
		t.Fatal("shift+tab then ctrl+t did not return to the starting session")
	}
	for _, ctx := range []Context{CtxPrompt, CtxScrollback, CtxDashboard} {
		if got := resolve("shift+tab", ctx); got != CmdPrevSession {
			t.Fatalf("shift+tab in %v resolves to %v, want CmdPrevSession", ctx, got)
		}
	}
}

// TestTheWheelOverTheSessionStripCyclesSessions.
func TestTheWheelOverTheSessionStripCyclesSessions(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	drawAt(a, 100, 30)
	if a.tabRow.Empty() {
		t.Fatal("two sessions drew no session strip")
	}
	first := a.active
	a.Dispatch(ActionScroll{X: a.tabRow.X + 1, Y: a.tabRow.Y, Delta: 3})
	if a.active == first {
		t.Fatal("the wheel over the session strip changed nothing")
	}
	a.Dispatch(ActionScroll{X: a.tabRow.X + 1, Y: a.tabRow.Y, Delta: -3})
	if a.active != first {
		t.Fatalf("wheel down then up left the strip on session %d", a.active)
	}

	// Over the transcript the wheel must not cycle sessions.
	sb := a.Current().Scroll
	a.Dispatch(ActionScroll{X: sb.view.X + 1, Y: sb.view.Y + 1, Delta: 3})
	if a.active != first {
		t.Fatal("the wheel over the transcript cycled sessions")
	}
}

// TestTheDashboardFollowsThePointerOnHover — the parity half of the pointer
// grammar the overlays already had.
func TestTheDashboardFollowsThePointerOnHover(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	key(a, "ctrl+g")
	drawAt(a, 100, 30)

	row := a.dashboard.rows[0]
	if !a.dashboard.HoverAt(row.X+1, row.Y) {
		t.Fatal("the first hover over a session row reported no change")
	}
	if a.dashboard.hover != 1 {
		t.Fatalf("the hover is %d, want row 0 (stored as 1)", a.dashboard.hover)
	}
	if a.dashboard.HoverAt(row.X+2, row.Y) {
		t.Fatal("a second hover on the same row reported a change — that repaints the frame")
	}
	// The hover and the selection stay apart: Enter opens the selection.
	if a.dashboard.Selected() != 1 {
		t.Fatalf("hovering moved the selection to %d", a.dashboard.Selected())
	}
	// A hover that leaves the rows clears the highlight.
	a.dashboard.HoverAt(0, 0)
	if a.dashboard.hover != 0 {
		t.Fatalf("hovering off the rows left the hover at %d", a.dashboard.hover)
	}
}

// TestANonLeftClickOnTheDashboardSelectsNothing.
func TestANonLeftClickOnTheDashboardSelectsNothing(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	key(a, "ctrl+g")
	a.dashboard.Select(0)
	drawAt(a, 100, 30)

	row := a.dashboard.rows[1]
	a.Dispatch(ActionClick{X: row.X + 1, Y: row.Y, Button: 3})
	if a.dashboard.Selected() != 0 {
		t.Fatalf("a right-click selected %d", a.dashboard.Selected())
	}
	if a.mode != ModeDashboard {
		t.Fatal("a right-click opened the session")
	}
}

// TestTheDashboardDrawsWhenItIsOneRowTall is the degenerate end of the
// windowing arithmetic: a frame with room for no full row must draw nothing
// and record nothing, not index a row that is not there.
func TestTheDashboardDrawsWhenItIsOneRowTall(t *testing.T) {
	a, _ := newTestApp()
	key(a, "ctrl+g")
	a.Dispatch(ActionResize{W: 60, H: 3})
	b := render.NewBuffer(60, 3)
	a.Draw(b) // must not panic
	a.Dispatch(ActionClick{X: 5, Y: 2, Button: 1})
	if a.dashboard.Selected() != 0 {
		t.Fatalf("a click under an undrawable list selected %d", a.dashboard.Selected())
	}
}

// TestResizingAwayAndBackKeepsTheDashboardRowsHonest: the row record must not
// outlive the geometry it describes.
func TestResizingAwayAndBackKeepsTheDashboardRowsHonest(t *testing.T) {
	host := &stubHost{}
	a := NewApp(Dark(), host)
	a.Dispatch(ActionResize{W: 100, H: 30})
	for i := 0; i < 8; i++ {
		a.AddSession("S"+itoa(i), "session "+itoa(i))
	}
	key(a, "ctrl+g")
	drawAt(a, 100, 30)
	drawn := len(a.dashboard.rows)

	// Shrink to nothing and come back.
	a.Dispatch(ActionResize{W: 100, H: 1})
	a.Draw(render.NewBuffer(100, 1))
	if len(a.dashboard.rows) != 0 {
		t.Fatalf("a 1-row dashboard kept %d row rects", len(a.dashboard.rows))
	}
	a.Dispatch(ActionResize{W: 100, H: 30})
	drawAt(a, 100, 30)
	if len(a.dashboard.rows) != drawn {
		t.Fatalf("after a resize round-trip the dashboard records %d rows, want %d",
			len(a.dashboard.rows), drawn)
	}
}

// TestPageKeysReachTheTranscriptUnchanged is the disruption check for the new
// CmdPageUp/CmdPageDown dispatcher cases: in transcript focus they resolve to

// TestARemovedSessionLeavesNoClickableGhost. The randomized pointer fuzz found
// this: a session removed between frames left its row in the dashboard's
// geometry record, and a click on where it had been selected an index the
// session list no longer held — one Enter or ctrl+x later, an out-of-range
// access. Membership changes now invalidate the record.
func TestARemovedSessionLeavesNoClickableGhost(t *testing.T) {
	a, _ := newTestApp()
	a.AddSession("S2", "session two")
	key(a, "ctrl+g")
	drawAt(a, 100, 30)
	ghost := a.dashboard.rows[1] // session two's row as drawn

	a.RemoveSession("S2")
	if len(a.dashboard.rows) != 0 {
		t.Fatal("removing a session kept the dashboard's row record")
	}
	// A click on the ghost's cells answers nothing rather than indexing a
	// session that no longer exists.
	a.Dispatch(ActionClick{X: ghost.X + 1, Y: ghost.Y, Button: 1})
	if a.dashboard.Selected() != 0 {
		t.Fatalf("a click on a removed session's row selected %d", a.dashboard.Selected())
	}
	// And the destructive path over that selection is in-bounds.
	effects := a.closeSelected()
	if len(effects) != 1 {
		t.Fatalf("closeSelected produced %#v", effects)
	}
	if e := effects[0].(EffectCloseSession); e.SessionID != "S1" {
		t.Fatalf("closeSelected targeted %q, want the surviving session", e.SessionID)
	}
}

// the same commands the scrollback has always answered, and they must still
// be answered there.
func TestPageKeysReachTheTranscriptUnchanged(t *testing.T) {
	a, _ := newTestApp()
	addEntries(a, 40)
	drawAt(a, 100, 30)
	key(a, "tab") // transcript focus
	sb := a.Current().Scroll

	key(a, "pgup")
	if sb.Following() {
		t.Fatal("pgup in the transcript did not scroll it up")
	}
	key(a, "g")
	if sb.Following() {
		t.Fatal("g in the transcript did not go to the top")
	}
	key(a, "G")
	if !sb.Following() {
		t.Fatal("G in the transcript did not return to the newest row")
	}
}

// TestNoKeyCombinationAnswersAnApprovalCardThroughTheNewBindings — the safety
// re-audit: the approval context gained nothing, and the keys the round added
// elsewhere cannot decide the card from behind it.
func TestNoKeyCombinationAnswersAnApprovalCardThroughTheNewBindings(t *testing.T) {
	for _, binding := range []string{"pgup", "pgdn", "g", "G", "home", "end", "left", "right", "h", "l", "shift+tab"} {
		a, _ := newTestApp()
		reply := raiseApproval(a, true)
		effects := key(a, binding)
		select {
		case d := <-reply:
			t.Fatalf("%q answered the approval card with %+v", binding, d)
		default:
		}
		for _, e := range effects {
			if _, ok := e.(EffectDecide); ok {
				t.Fatalf("%q produced a decision", binding)
			}
		}
		if a.Current().Approval() == nil {
			t.Fatalf("%q dismissed the card without deciding it", binding)
		}
	}
}
