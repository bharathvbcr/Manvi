package tui

import (
	"time"

	"manvi/ui/fx"
	"manvi/ui/render"
)

// Dashboard is the AppView-level surface: every session at once.
//
// It exists because the harness fans out. A planner spawns search agents and an
// orchestrator spawns builders, each holding a lease on a task, and the
// question an operator actually has is not "what is this session doing" but
// "which of these is blocked on me, which is burning tokens, and which is
// holding a lease that is about to lapse". A single-session view cannot answer
// any of those.
type Dashboard struct {
	sel int
}

// Clamp keeps the selection inside the list.
func (d *Dashboard) Clamp(n int) {
	if d.sel >= n {
		d.sel = n - 1
	}
	if d.sel < 0 {
		d.sel = 0
	}
}

// Move changes the highlighted session.
func (d *Dashboard) Move(delta, n int) {
	if n == 0 {
		return
	}
	d.sel = (d.sel + delta + n) % n
}

// Selected is the highlighted index.
func (d *Dashboard) Selected() int { return d.sel }

// Select sets the highlighted index.
func (d *Dashboard) Select(i int) { d.sel = i }

// rowHeight is how many rows one session occupies.
const rowHeight = 2

// HitTest maps a click row to a session index, or -1.
func (d *Dashboard) HitTest(r render.Rect, y, n int) int {
	body := r.Pad(3, 2, 2, 2)
	idx := (y - body.Y) / rowHeight
	if idx < 0 || idx >= n {
		return -1
	}
	return idx
}

// Draw paints the session list.
//
// fxOn animates the header: the mark carries a wave of the gradient's second
// colour, and the blocked count breathes — the two header elements an
// operator glances at, given life without moving a single cell of the list
// itself.
func (d *Dashboard) Draw(b *render.Buffer, r render.Rect, th Theme, views []*AgentView, active string, tick int, fxOn bool) {
	if r.Empty() {
		return
	}
	g := th.Glyphs()
	b.Fill(r, ' ', th.Base())

	area := r
	header, area := area.SplitTop(2)
	title := render.Line{}
	switch {
	case fxOn && th.Unicode && th.Name != "plain":
		title = fx.GradientSweep("  manvi", th.Accent, th.Info, tick, render.Bold)
	case th.Unicode && th.Name != "plain":
		title = render.GradientLine("  manvi", th.Accent, th.Info, render.Bold)
	default:
		title = render.Styled("  manvi", th.AccentStyle())
	}
	title = title.Append("  agent dashboard", th.Muted()).
		Append("   "+itoa(len(views))+" session(s)", th.Subtle())
	title.DrawIn(b, render.Rect{X: header.X, Y: header.Y, W: header.W, H: 1})

	// A blocked session is the only thing on this screen that needs an
	// operator's hands, so it is counted in the header rather than only marked
	// in its row.
	blocked := 0
	for _, v := range views {
		if v.Approval() != nil {
			blocked++
		}
	}
	if blocked > 0 {
		blockedStyle := th.Status(StatusWarn).With(render.Bold)
		if fxOn {
			// The one number on this screen that asks for hands: it breathes.
			blockedStyle.Fg = th.Warning.Blend(th.FgOn, fx.Pulse(tick, 18)*0.5)
		}
		render.Styled("  "+g.Warn+" "+itoa(blocked)+" session(s) blocked on you", blockedStyle).
			DrawIn(b, render.Rect{X: header.X, Y: header.Y + 1, W: header.W, H: 1})
	}

	area, hints := area.SplitBottom(1)
	body := area.Pad(1, 2, 1, 2)

	if len(views) == 0 {
		render.Styled("no sessions — ctrl+n to start one", th.Subtle()).
			DrawIn(b, render.Rect{X: body.X, Y: body.Y, W: body.W, H: 1})
		DrawShortcutBar(b, hints, th, CtxDashboard)
		return
	}

	now := time.Now()
	for i, v := range views {
		y := body.Y + i*rowHeight
		if y+1 >= body.Bottom() {
			render.Styled("… "+itoa(len(views)-i)+" more", th.Subtle()).
				DrawIn(b, render.Rect{X: body.X, Y: y, W: body.W, H: 1})
			break
		}
		selected := i == d.sel
		rowRect := render.Rect{X: r.X, Y: y, W: r.W, H: rowHeight}
		if selected {
			b.Fill(rowRect, ' ', render.Style{Bg: th.Selection})
		}

		bg := th.Bg
		if selected {
			bg = th.Selection
		}
		nameStyle := render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Bold}
		metaStyle := render.Style{Fg: th.FgMuted, Bg: bg}

		// State chip.
		chip, chipStyle := "idle", render.Style{Fg: th.FgSubtle, Bg: bg}
		switch {
		case v.Approval() != nil:
			chip = "blocked"
			chipStyle = render.Style{Fg: th.FgOn, Bg: th.Warning, Attrs: render.Bold}
		case v.Error() != "":
			chip = "error"
			chipStyle = render.Style{Fg: th.FgOn, Bg: th.Danger, Attrs: render.Bold}
		case v.Status.Busy:
			chip = render.Spinner(g.Spinner, tick) + " busy"
			chipStyle = render.Style{Fg: th.Accent, Bg: bg, Attrs: render.Bold}
		}

		marker := "  "
		if v.ID == active {
			marker = g.Caret + " "
		}
		line := render.Styled(marker, render.Style{Fg: th.Accent, Bg: bg}).
			Append(render.PadWidth(" "+chip+" ", 9), chipStyle).
			Append(" ", metaStyle).
			Append(render.TruncateWidth(v.Title, 32, "…"), nameStyle)
		if v.Status.TaskID != "" {
			line = line.Append("  "+v.Status.TaskID, render.Style{Fg: th.Info, Bg: bg})
		}
		line.Truncate(body.W).Draw(b, body.X, y)

		// Second row: the numbers that decide where attention goes.
		meta := render.Styled("     ", metaStyle)
		if v.Status.Model != "" {
			meta = meta.Append(v.Status.Model+"  ", metaStyle)
		}
		if v.Status.InputTokens > 0 || v.Status.OutputTokens > 0 {
			meta = meta.Append(compactCount(v.Status.InputTokens)+"↓ "+
				compactCount(v.Status.OutputTokens)+"↑  ", metaStyle)
		}
		if v.Status.Grants > 0 {
			meta = meta.Append(g.Granted+" "+itoa(v.Status.Grants)+" granted  ",
				render.Style{Fg: th.Granted, Bg: bg, Attrs: render.Bold})
		}
		if len(v.Status.Degraded) > 0 {
			meta = meta.Append(g.Degraded+" "+itoa(len(v.Status.Degraded))+" unrun  ",
				render.Style{Fg: th.Degraded, Bg: bg})
		}
		if !v.Activity().IsZero() {
			meta = meta.Append(shortDuration(now.Sub(v.Activity()))+" ago",
				render.Style{Fg: th.FgSubtle, Bg: bg})
		}
		if v.Error() != "" {
			meta = meta.Append("  "+render.TruncateWidth(v.Error(), 40, "…"),
				render.Style{Fg: th.Danger, Bg: bg})
		}
		meta.Truncate(body.W).Draw(b, body.X, y+1)
	}

	DrawShortcutBar(b, hints, th, CtxDashboard)
}
