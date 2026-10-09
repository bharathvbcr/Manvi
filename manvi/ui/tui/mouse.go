package tui

import (
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

func (a *App) scroll(t ActionScroll) []Effect {
	// The overlay comes first and unconditionally: it is the topmost
	// surface, and gating its wheel on a session existing made the palette
	// wheel-dead on the empty startup screen — the one screen where the
	// palette is the way forward.
	if a.overlay != nil {
		a.overlay.Move(sign(t.Delta))
		return nil
	}
	if a.mode == ModeDashboard {
		a.dashboard.Move(sign(t.Delta), len(a.views))
		return nil
	}
	v := a.Current()
	if v == nil {
		return nil
	}
	if card := v.Approval(); card != nil {
		// The card is modal for the wheel exactly as it is for keys and
		// clicks: a notch moves the highlight between options, and the
		// transcript behind an unanswered question does not scroll.
		card.Next(sign(t.Delta))
		return nil
	}
	// The session strip is a control, not content: a wheel notch over it
	// cycles sessions, as it does over a browser's tab strip.
	if len(a.views) > 1 && a.tabRow.Contains(t.X, t.Y) {
		return a.cycleSession(sign(t.Delta))
	}
	v.Scroll.ScrollBy(t.Delta)
	return nil
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

// click routes a pointer press. The grammar is uniform across surfaces, so
// the mouse is learnable from any one of them: a click moves the highlight,
// and a click on the highlighted thing — a second click — confirms it. The
// keyboard's two-step (arrows then Enter) and the pointer's are the same
// shape, which is also what keeps a stray trackpad brush from answering
// anything.
func (a *App) click(t ActionClick) []Effect {
	// An open overlay owns the pointer entirely. Clicks were dropped on the
	// floor here once, which made the palette, the pickers, and the help list
	// keyboard-only surfaces.
	if a.overlay != nil {
		o := a.overlay
		if !a.overlayRect.Contains(t.X, t.Y) {
			// Click-away dismisses, as Esc does — with the left button. Any
			// other press outside the frame is swallowed rather than
			// dismissed: it is as likely a mis-aim as an intent, and the
			// list costs a keystroke to reopen.
			if t.Button == 1 {
				a.overlay = nil
			}
			return nil
		}
		if t.Button != 1 {
			// Inside the frame a non-left press changes nothing. The
			// grammar is "left-click moves the highlight, left-click again
			// confirms", and a confirmation a middle-click can trigger is
			// one the ledger cannot tell from a deliberate one — the row it
			// would accept can be a mutating command.
			return nil
		}
		idx := o.ItemAt(t.X, t.Y)
		if idx < 0 {
			// The border, title, or query field: swallowed, not dismissed —
			// a click that misses a row by one cell must not close the list.
			return nil
		}
		if o.Sel() == idx {
			// Second click on the highlighted row accepts it.
			return a.accept()
		}
		o.MoveTo(idx)
		return nil
	}

	if a.mode == ModeDashboard {
		if t.Button != 1 {
			// Selection and open are left-button verbs, as they are in the
			// overlays.
			return nil
		}
		idx := a.dashboard.HitTest(t.X, t.Y)
		if idx < 0 {
			return nil
		}
		if idx == a.dashboard.Selected() && t.Button == 1 && a.doubleClick(t.X, t.Y) {
			// Second click on the highlighted row opens the session.
			a.active = idx
			a.mode = ModeSession
			return nil
		}
		a.doubleClick(t.X, t.Y)
		a.dashboard.Select(idx)
		return nil
	}

	v := a.Current()
	if v == nil {
		return nil
	}

	// The session strip is app chrome, above the session's own layout.
	if t.Button == 1 && len(a.tabs) > 0 {
		for _, tab := range a.tabs {
			if !tab.Rect.Contains(t.X, t.Y) {
				continue
			}
			if tab.IsNewButton {
				return []Effect{EffectNewSession{}}
			}
			if tab.SessionIndex != a.active {
				a.active = tab.SessionIndex
			}
			return nil
		}
		if a.tabRow.Contains(t.X, t.Y) {
			return nil
		}
	}

	// An approval card is modal for the pointer exactly as it is for the
	// keyboard: until it is answered, clicks belong to it and to nothing
	// behind it.
	if card := v.Approval(); card != nil {
		accept, _ := card.Click(t.X, t.Y)
		if !accept {
			return nil
		}
		return a.acceptApproval()
	}

	// The composer: click focuses it and puts the caret where the click
	// landed, the courtesy every text field extends.
	if t.Button == 1 && v.promptRect.Contains(t.X, t.Y) {
		v.Focus = CtxPrompt
		v.Scroll.SelectNone()
		v.Prompt.SetCursor(v.Prompt.IndexAt(v.promptRect.W,
			t.Y-v.promptRect.Y, t.X-v.promptRect.X))
		return nil
	}

	// The scrollbar column is a control, not content: a press on it grabs the
	// thumb and the drag scrolls, rather than selecting transcript text. It
	// sits beside the content rect, so it is tested before the content is.
	if bar := v.Scroll.ScrollbarCol(); t.Button == 1 && bar >= 0 && t.X == bar &&
		t.Y >= v.Scroll.view.Y && t.Y < v.Scroll.view.Bottom() {
		a.dragBar = true
		v.Scroll.ScrollToFraction(scrollFraction(t.Y, v.Scroll.view))
		return nil
	}

	// Hit-tested against the rect the last frame actually drew, rather than
	// against one recomputed here. Recomputing means two versions of the layout
	// arithmetic, and the one in a click handler is the one that goes stale.
	if v.Scroll.Contains(t.X, t.Y) {
		v.Focus = CtxScrollback
		if t.Button == 3 {
			// Right-click copies the entry under the pointer, matching y.
			if v.Scroll.SelectAt(t.Y) {
				if text := v.Scroll.SelectedText(); text != "" {
					a.setNotice("copied "+itoa(len(text))+" bytes", StatusInfo)
					return []Effect{EffectCopy{Text: text}}
				}
			}
			return nil
		}
		if v.Scroll.SelectAt(t.Y) && a.doubleClick(t.X, t.Y) {
			v.Scroll.ToggleFold()
		}
		return nil
	}

	v.Focus = CtxPrompt
	v.Scroll.SelectNone()
	return nil
}

// motion handles the pointer moving: a scrollbar drag in progress, or a hover
// crossing the rows of whatever list is on top.
func (a *App) motion(t ActionMotion) []Effect {
	if a.dragBar {
		if v := a.Current(); v != nil && v.Scroll.ScrollbarCol() >= 0 {
			v.Scroll.ScrollToFraction(scrollFraction(t.Y, v.Scroll.view))
		}
		return nil
	}
	if t.Button != 0 {
		return nil
	}
	if a.overlay != nil {
		a.overlay.HoverAt(t.X, t.Y)
		return nil
	}
	if a.mode == ModeDashboard {
		a.dashboard.HoverAt(t.X, t.Y)
	}
	return nil
}

// scrollFraction maps a row inside the transcript viewport to a fraction of
// the scroll travel, for a dragged thumb.
func scrollFraction(y int, view render.Rect) float64 {
	if view.H <= 1 {
		return 0
	}
	return float64(y-view.Y) / float64(view.H-1)
}

// doubleClick reports whether this press is the second at the same cell
// within the confirmation window, and records it either way.
func (a *App) doubleClick(x, y int) bool {
	now := time.Now()
	again := x == a.lastClickX && y == a.lastClickY && now.Sub(a.lastClick) < DoublePress
	a.lastClick, a.lastClickX, a.lastClickY = now, x, y
	return again
}
