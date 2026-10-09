package tui

import (
	"github.com/bharathvbcr/Manvi/manvi/ui/fx"
	"github.com/bharathvbcr/Manvi/manvi/ui/logo"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// Draw paints the whole frame and returns the caret position.
func (a *App) Draw(b *render.Buffer) render.Rect {
	w, h := b.W, b.H
	a.width, a.height = w, h
	full := render.Rect{X: 0, Y: 0, W: w, H: h}
	b.Fill(full, ' ', a.Theme.Base())
	a.overlayRect = render.Rect{}

	var caret render.Rect
	switch {
	case a.mode == ModeDashboard:
		a.tabs = nil
		a.tabRow = render.Rect{}
		active := ""
		if v := a.Current(); v != nil {
			active = v.ID
		}
		a.dashboard.Draw(b, full, a.Theme, a.views, active, a.tick, a.fxOn())
	case a.Current() != nil:
		area := full
		if len(a.views) > 1 {
			// The session strip takes a row of its own only when there is
			// more than one session to switch between — a single session
			// keeps its transcript row rather than losing it to a strip that
			// says nothing.
			a.tabRow, area = area.SplitTop(1)
			a.tabs = DrawSessionTabBar(b, a.tabRow, a.Theme, a.views, a.active, a.tick)
		} else {
			a.tabs = nil
			a.tabRow = render.Rect{}
		}
		caret = a.Current().Draw(b, area, a.Theme, a.tick, a.series.Values(), a.fxOn())
	default:
		a.tabs = nil
		a.tabRow = render.Rect{}
		a.drawEmpty(b, full)
	}

	if a.notice != "" {
		a.drawNotice(b, full)
	}
	if a.overlay != nil {
		if c := a.drawOverlay(b, full); c.W > 0 {
			caret = c
		} else {
			caret = render.Rect{}
		}
	}
	return caret
}

func (a *App) drawEmpty(b *render.Buffer, r render.Rect) {
	th := a.Theme
	blocks := th.LogoBlocks()

	// The idle backdrop: a rain field, drawn first so the mark and the hint
	// paint over it. Procedural per the fx package's rules — a pure function
	// of the tick — and absent entirely when animation is off or the terminal
	// cannot carry the colour.
	if a.fxOn() {
		fx.Rain{Gap: 3, Tail: 9}.Draw(b, r, a.tick, th.AccentDim, th.Bg, th.Unicode)
	}

	// The mark is centred on the pane rather than pinned to a row, and it
	// shrinks through its own rungs, so a narrow split pane gets a smaller mark
	// instead of a corrupted frame.
	size := logo.Fit(r.W, r.H, blocks)
	top := r.Y + (r.H-size.Height())/2 - 1
	if top < r.Y {
		top = r.Y
	}
	used := logo.Draw(b, render.Rect{X: r.X, Y: top, W: r.W, H: r.H - (top - r.Y)},
		th.Logo(), blocks, "the DevCouncil execution harness")

	// The hint types itself out once at startup — a first-run cue that the
	// keys on it are live, timed to finish inside the first couple of seconds.
	hintText := "ctrl+n  new session      ctrl+p  commands      ctrl+q  quit"
	if a.fxOn() {
		hintText = fx.RevealText(hintText, a.tick, 0, 4)
	}
	hint := render.Styled(hintText, th.Subtle())
	if y := top + used + 1; y < r.Bottom() && hintText != "" {
		hint.Truncate(r.W).Draw(b, r.X+(r.W-hint.Width())/2, y)
	}
}

func (a *App) drawNotice(b *render.Buffer, r render.Rect) {
	th := a.Theme
	style := th.Status(a.noticeKind)
	style.Bg = th.BgRaised
	text := " " + a.notice + " "
	w := render.StringWidth(text)
	if w > r.W-4 {
		w = r.W - 4
	}
	x := r.Right() - w - 2

	// Below the status bar, and below the strict banner when there is one. A
	// transient message may cover transcript, which scrolls back; it may not
	// cover the statement that this run's results are not strict, which is the
	// one thing on screen with nowhere else to be read.
	y := r.Y + 1
	if v := a.Current(); v != nil {
		if strict, _ := v.Status.StrictSummary(); !strict {
			y = r.Y + 2
		}
	}
	if y >= r.Bottom() {
		y = r.Bottom() - 1
	}
	if x < r.X {
		x = r.X
	}
	b.Fill(render.Rect{X: x, Y: y, W: w, H: 1}, ' ', style)
	render.Styled(text, style).Truncate(w).Draw(b, x, y)
}

func (a *App) drawOverlay(b *render.Buffer, r render.Rect) render.Rect {
	o := a.overlay
	// The rect is recorded as well as drawn, so the click handler tests
	// against the frame the operator is looking at rather than recomputing it.
	switch o.Kind {
	case OverlayComplete:
		// Anchored just above the composer, where the text being completed is.
		h := o.Height(10)
		w := r.W / 2
		if w < 40 {
			w = r.W - 4
		}
		v := a.Current()
		promptH := 3
		if v != nil {
			promptH = v.promptHeight(r.W) + 2
		}
		y := r.Bottom() - 1 - promptH - h
		if y < r.Y {
			y = r.Y
		}
		a.overlayRect = render.Rect{X: r.X + 2, Y: y, W: w, H: h}
	default:
		h := o.Height(r.H - 6)
		w := r.W * 2 / 3
		if w < 50 {
			w = r.W - 4
		}
		if w > r.W-4 {
			w = r.W - 4
		}
		x := r.X + (r.W-w)/2
		y := r.Y + (r.H-h)/3
		a.overlayRect = render.Rect{X: x, Y: y, W: w, H: h}
	}
	return o.Draw(b, a.overlayRect, a.Theme)
}
