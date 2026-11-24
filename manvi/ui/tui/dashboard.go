package tui

import (
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui/fx"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
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
	// hover is the row under the pointer plus one, so the zero value means
	// "no hover". It is drawn apart from the selection for the same reason
	// the overlays keep the two apart: the pointer shows where a click would
	// land, the selection is what Enter opens, and conflating them means the
	// highlight jumps away from a keyboard user's own navigation every time
	// the mouse moves.
	hover int
	// rows are the session rows as the last frame drew them, in session
	// order. Hit-testing reads this record rather than recomputing the
	// layout — two copies of the same arithmetic are how a click lands on
	// the row beside the one under the pointer, and the recomputed copy is
	// the one that goes stale.
	rows []render.Rect
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

// Invalidate drops the recorded geometry because the session list changed.
// The record describes the frame that drew it; a session added or removed
// makes that frame a lie about the index a click would select, so the record
// dies with the membership and the next draw rebuilds it.
func (d *Dashboard) Invalidate() {
	d.rows = nil
	d.hover = 0
}

// Page moves the highlight by a screenful of rows, for pgup/pgdn. The page is
// the count of rows the last frame fit, so a page key moves exactly as far as
// the screen it was pressed on showed; before the first frame it falls back
// to a single step rather than doing nothing.
func (d *Dashboard) Page(delta, n int) {
	page := len(d.rows)
	if page < 1 {
		page = 1
	}
	d.Move(delta*page, n)
}

// HoverAt moves the pointer highlight to the row under (x,y), reporting
// whether it moved. The caller repaints only on a true return, so a pointer
// crossing cells within one row costs no frame.
func (d *Dashboard) HoverAt(x, y int) bool {
	idx := d.HitTest(x, y) + 1
	if idx == d.hover {
		return false
	}
	d.hover = idx
	return true
}

// HitTest maps a point to the session whose row it is on, or -1.
//
// It reads the rows the last frame drew rather than recomputing the layout.
// The overflow line ("… N more") is not a row, so a click on it hits nothing
// — when this was computed from padding arithmetic it resolved to a session
// that had never been on screen, and a click there selected that session
// sight unseen.
func (d *Dashboard) HitTest(x, y int) int {
	for i, r := range d.rows {
		if r.Contains(x, y) {
			return i
		}
	}
	return -1
}

// rowHeight is how many rows one session occupies.
const rowHeight = 2

// dashboardMinSplitWidth is the narrowest terminal width that splits the
// dashboard into a 55% session list and 45% live preview pane.
const dashboardMinSplitWidth = 116

// Draw paints the session list.
//
// fxOn animates the header: the mark carries a wave of the gradient's second
// colour, and the blocked count breathes — the two header elements an
// operator glances at, given life without moving a single cell of the list
// itself.
func (d *Dashboard) Draw(b *render.Buffer, r render.Rect, th Theme, views []*AgentView, active string, tick int, fxOn bool) {
	// Nothing drawn means nothing hit-testable: the record must not outlive
	// the frame it describes.
	d.rows = d.rows[:0]
	if r.Empty() {
		return
	}
	g := th.Glyphs()
	b.Fill(r, ' ', th.Base())

	area := r
	header, area := area.SplitTop(2)
	var title render.Line
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
			blockedStyle.Fg = th.Fg.Blend(th.FgSubtle, fx.Pulse(tick, 18)*0.5)
		}
		render.Styled("  "+g.Warn+" "+itoa(blocked)+" session(s) blocked on you", blockedStyle).
			DrawIn(b, render.Rect{X: header.X, Y: header.Y + 1, W: header.W, H: 1})
	}

	area, hints := area.SplitBottom(1)

	if len(views) == 0 {
		body := area.Pad(1, 2, 1, 2)
		render.Styled("no sessions — ctrl+n to start one", th.Subtle()).
			DrawIn(b, render.Rect{X: body.X, Y: body.Y, W: body.W, H: 1})
		DrawShortcutBar(b, hints, th, CtxDashboard)
		return
	}

	listArea := area
	var divArea, previewArea render.Rect
	if r.W >= dashboardMinSplitWidth {
		leftW := area.W * 55 / 100
		var rightArea render.Rect
		listArea, rightArea = area.SplitLeft(leftW)
		divArea, previewArea = rightArea.SplitLeft(1)
	}
	body := listArea.Pad(1, 2, 1, 2)

	now := time.Now()

	// The window follows the selection, so a fleet wider than the screen
	// stays navigable: without it the highlight — and the session Enter
	// opens — walks off the bottom of the list into rows nobody can see,
	// and the overflow line stands in for sessions the dashboard exists to
	// show.
	visible := body.H / rowHeight
	if visible < 1 {
		visible = 1
	}
	top := 0
	if d.sel >= visible {
		top = d.sel - visible + 1
	}
	if top > 0 {
		render.Styled("… "+itoa(top)+" above", th.Subtle()).
			DrawIn(b, render.Rect{X: body.X, Y: body.Y - 1, W: body.W, H: 1})
	}

	for i := top; i < len(views); i++ {
		v := views[i]
		y := body.Y + (i-top)*rowHeight
		if y+1 >= body.Bottom() {
			render.Styled("… "+itoa(len(views)-i)+" more", th.Subtle()).
				DrawIn(b, render.Rect{X: body.X, Y: y, W: body.W, H: 1})
			break
		}
		selected := i == d.sel
		hovered := d.hover == i+1
		rowRect := render.Rect{X: listArea.X, Y: y, W: listArea.W, H: rowHeight}
		// Recorded for the pointer: a click is tested against the rows as
		// they were drawn, window offset included.
		d.rows = append(d.rows, rowRect)
		if selected {
			b.Fill(rowRect, ' ', render.Style{Bg: th.Selection})
		} else if hovered {
			b.Fill(rowRect, ' ', render.Style{Bg: th.BgInset})
		}

		bg := th.Bg
		if selected {
			bg = th.Selection
		} else if hovered {
			bg = th.BgInset
		}
		nameStyle := render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Bold}
		metaStyle := render.Style{Fg: th.FgMuted, Bg: bg}

		// State chip.
		chip, chipStyle := "idle", render.Style{Fg: th.FgSubtle, Bg: bg}
		switch {
		case v.Approval() != nil:
			chip = "blocked"
			chipStyle = render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Bold}
		case v.Error() != "":
			chip = "error"
			chipStyle = render.Style{Fg: th.Danger, Bg: bg, Attrs: render.Bold}
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
				render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Bold})
		}
		if len(v.Status.Degraded) > 0 {
			meta = meta.Append(g.Degraded+" "+itoa(len(v.Status.Degraded))+" unrun  ",
				render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Italic | render.Underline})
		}
		if !v.Activity().IsZero() {
			// shortDuration answers "just now" under a second, which is already
			// a whole phrase: with the suffix it read "just now ago".
			age := shortDuration(now.Sub(v.Activity()))
			if age != "just now" {
				age += " ago"
			}
			meta = meta.Append(age, render.Style{Fg: th.FgSubtle, Bg: bg})
		}
		if v.Error() != "" {
			meta = meta.Append("  "+render.TruncateWidth(v.Error(), 40, "…"),
				render.Style{Fg: th.Danger, Bg: bg})
		}
		meta.Truncate(body.W).Draw(b, body.X, y+1)
	}

	if !divArea.Empty() && body.H > 0 {
		divGlyph := "│"
		if !th.Unicode {
			divGlyph = "|"
		}
		divStyle := render.Style{Fg: th.ruleColor(), Bg: th.Bg}
		for y := divArea.Y; y < divArea.Bottom(); y++ {
			b.SetString(divArea.X, y, divGlyph, divStyle)
		}
	}
	if !previewArea.Empty() && body.H > 0 && d.sel >= 0 && d.sel < len(views) {
		d.drawPreview(b, previewArea, th, views[d.sel], tick, now)
	}

	DrawShortcutBar(b, hints, th, CtxDashboard)
}

func (d *Dashboard) drawPreview(b *render.Buffer, r render.Rect, th Theme, v *AgentView, tick int, now time.Time) {
	if v == nil || r.Empty() {
		return
	}
	pane := r.Pad(1, 2, 1, 2)
	if pane.Empty() || pane.W < 12 {
		return
	}
	g := th.Glyphs()
	var lines []render.Line

	stateChip, stateStyle := "idle", th.Subtle()
	switch {
	case v.Approval() != nil:
		stateChip, stateStyle = "blocked", th.Status(StatusWarn).With(render.Bold)
	case v.Error() != "":
		stateChip, stateStyle = "error", th.Status(StatusBlock).With(render.Bold)
	case v.Status.Busy:
		stateChip, stateStyle = render.Spinner(g.Spinner, tick)+" busy", th.AccentStyle()
	}
	title := v.Title
	if title == "" {
		title = v.ID
	}
	lines = append(lines, render.Styled(title+"  ", th.Strong()).Append("["+stateChip+"]", stateStyle))

	hBar := "─"
	if !th.Unicode {
		hBar = "-"
	}
	lines = append(lines, render.Styled(strings.Repeat(hBar, pane.W), render.Style{Fg: th.ruleColor(), Bg: th.Bg}))

	st := v.Status
	strict, why := st.StrictSummary()
	postureStyle := th.Strong()
	if !strict {
		postureStyle = th.Status(StatusWarn).With(render.Bold)
	}
	postureLine := render.Styled("Posture  ", th.Subtle()).Append(orDash(st.Posture), postureStyle)
	if !strict && why != "" {
		postureLine = postureLine.Append(" ("+why+")", th.Status(StatusWarn))
	}
	lines = append(lines, postureLine)

	modelText := orDash(st.Model)
	if st.InputTokens > 0 || st.OutputTokens > 0 {
		modelText += " · " + compactCount(st.InputTokens) + "↓ " + compactCount(st.OutputTokens) + "↑"
	}
	lines = append(lines, render.Styled("Model    ", th.Subtle()).Append(modelText, th.Base()))

	leaseText := "—"
	leaseStyle := th.Muted()
	if st.TaskID != "" {
		leaseText = st.TaskID
		leaseStyle = th.Status(StatusInfo)
	}
	grantLabel := itoa(st.Grants) + " grant(s)"
	grantStyle := th.Muted()
	if st.Grants > 0 {
		grantStyle = th.Status(StatusGranted)
	}
	lines = append(lines, render.Styled("Lease    ", th.Subtle()).
		Append(leaseText, leaseStyle).
		Append("  ·  ", th.Subtle()).
		Append(grantLabel, grantStyle))

	if len(st.Degraded) > 0 {
		lines = append(lines, render.Styled("Unrun    ", th.Subtle()).
			Append(strings.Join(st.Degraded, ", "), th.Status(StatusDegraded)))
	}

	lines = append(lines, render.Line{})

	if card := v.Approval(); card != nil {
		req := card.Request
		lines = append(lines, render.Styled(g.Warn+" Pending Approval", th.Status(StatusWarn).With(render.Bold)))
		if req.Rule != "" {
			ruleLine := render.Styled("Rule     ", th.Subtle()).Append(req.Rule, th.Strong())
			if req.Severity != "" {
				ruleLine = ruleLine.Append(" ["+req.Severity+"]", th.Status(StatusWarn))
			}
			lines = append(lines, ruleLine)
		}
		if req.Path != "" {
			lbl := "Path     "
			if req.Subject == "command" {
				lbl = "Command  "
			}
			lines = append(lines, render.Styled(lbl, th.Subtle()).Append(req.Path, th.Status(StatusInfo)))
		}
		if req.Reason != "" {
			lines = append(lines, render.WrapText("Reason: "+req.Reason, pane.W, th.Muted())...)
		}
		wait := "waiting " + shortDuration(now.Sub(card.arrived))
		if q := v.PendingApprovals(); q > 0 {
			wait += " · +" + itoa(q) + " queued"
		}
		lines = append(lines, render.Styled(wait, th.Subtle()))
		lines = append(lines, render.Styled("Press Enter to open session and answer", th.Subtle()))
	} else {
		if errText := v.Error(); errText != "" {
			lines = append(lines, render.Styled("Last Error", th.Status(StatusBlock).With(render.Bold)))
			lines = append(lines, render.WrapText(errText, pane.W, th.Status(StatusBlock))...)
			lines = append(lines, render.Line{})
		}
		lines = append(lines, render.Styled("Recent Transcript", th.Strong()))
		rows := recentTranscriptLines(v, th, pane.W, 8)
		if len(rows) == 0 {
			lines = append(lines, render.Styled("No transcript activity yet.", th.Subtle()))
		} else {
			lines = append(lines, rows...)
		}
	}

	for row := 0; row < pane.H && row < len(lines); row++ {
		lines[row].Truncate(pane.W).Draw(b, pane.X, pane.Y+row)
	}
}

func recentTranscriptLines(v *AgentView, th Theme, width, limit int) []render.Line {
	if v == nil || v.Scroll == nil || limit <= 0 {
		return nil
	}
	refs := v.Scroll.rows(th, width)
	var nonGap []render.Line
	for _, ref := range refs {
		if ref.gap && len(ref.line) == 0 {
			continue
		}
		nonGap = append(nonGap, ref.line)
	}
	if len(nonGap) > limit {
		nonGap = nonGap[len(nonGap)-limit:]
	}
	return nonGap
}
