package tui

import (
	"slices"
	"strings"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/ui/fx"
	"github.com/bharathvbcr/Manvi/manvi/ui/logo"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// Status is the persistent state the bar reports.
//
// Posture, weakened flags, and degraded checks are fields here rather than
// events in the transcript because a transcript scrolls. "A disabled safety
// flag is reported on every run that used it" only holds if it is reported
// somewhere that cannot scroll away — an operator who has to remember a banner
// from four hundred lines ago is an operator who will read a green result as an
// enforced one.
type Status struct {
	Posture string
	Model   string
	TaskID  string

	InputTokens  int
	OutputTokens int

	// Weakened names safety flags moved off their defaults.
	Weakened []string
	// Degraded names checks that could not run in this session.
	Degraded []string
	// Grants counts overrides issued, so a run that leaned on them is legible
	// at a glance rather than only in the ledger.
	Grants int

	Busy      bool
	BusyLabel string
	// BusySecs is how long the running turn has run, and StallSecs how long
	// since it last produced anything. Both are for the working row.
	BusySecs  int
	StallSecs int
	Sessions  int
}

// StrictSummary reports whether this session's results can be described as
// strict, and why not when they cannot.
//
// It returns a reason rather than a boolean alone, so the bar never has to
// invent an explanation for a warning it is showing.
func (s Status) StrictSummary() (bool, string) {
	var reasons []string
	if effect := flags.DescribePosture(s.Posture); effect.Relaxed {
		reasons = append(reasons, effect.Short)
	}
	if len(s.Weakened) > 0 {
		reasons = append(reasons, itoa(len(s.Weakened))+" safety flag(s) off default")
	}
	if len(s.Degraded) > 0 {
		reasons = append(reasons, itoa(len(s.Degraded))+" check(s) could not run")
	}
	if s.Grants > 0 {
		reasons = append(reasons, itoa(s.Grants)+" override(s) applied")
	}
	if len(reasons) == 0 {
		return true, ""
	}
	return false, strings.Join(reasons, "; ")
}

// DrawStatusBar paints the identity row: what is enforcing, which model, which
// task, and what the session has spent.
//
// It says what the session is, not what it is doing. Whether a turn is running
// is on the working row above the composer (DrawActivity), next to where the
// operator is typing, so this row does not change shape as turns start and end.
//
// Everything on it is a segment with a rank, and a bar too narrow for all of
// them drops whole segments, least important first. It used to draw the right
// cluster in full and cut the left to whatever remained, which at sixty
// columns left the posture, an ellipsis, and a row of counters — the
// model and the task gone, and a branch name cut to its first letter at a
// hundred. The posture is never dropped, and the grant count goes last of
// the rest: both say how far this run's results can be trusted.
//
// spark is the recent tokens-per-tick series, drawn as a sparkline while a
// turn runs on a bar wide enough to spare the room.
func DrawStatusBar(b *render.Buffer, r render.Rect, th Theme, st Status, tick int, spark []float64, fxOn bool) {
	if r.Empty() {
		return
	}
	g := th.Glyphs()
	// On the screen's own ground, in the two quietest text colours. It was a
	// raised bar across the top row with the model in bold: the first thing in
	// the frame, saying what does not change from one turn to the next. What
	// does need seeing — a grant, a full context window, a lease running out —
	// is still drawn strong, and is the only thing on the row that is.
	bg := th.Bg
	base := render.Style{Fg: th.FgSubtle, Bg: bg}
	name := render.Style{Fg: th.FgMuted, Bg: bg}
	strong := render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Bold}
	if th.reducedPalette() {
		// One dark grey is all a sixteen-colour terminal has for both.
		base = name
	}
	b.Fill(r, ' ', base)

	type segment struct {
		line render.Line
		// rank orders what goes first when the bar is too narrow: the highest
		// rank is dropped first, and rank 0 is never dropped.
		rank  int
		right bool
	}
	var segs []segment
	put := func(rank int, right bool, l render.Line) {
		segs = append(segs, segment{line: l, rank: rank, right: right})
	}

	// Posture is first, because it is the single fact that changes what
	// everything else on this bar means.
	// Three weights for three answers to "is this run enforcing?": quiet for
	// strict, which is the usual case; the text colour in bold for a posture
	// that still records what it demoted; the failure colour for one that has
	// stopped asking. yolo is not a louder dev — an operator glancing at the
	// bar has to be able to tell the two apart without reading.
	// It is a word, not a block of colour behind one: a filled chip was the
	// brightest thing on the screen and said one word.
	postureStyle := name
	switch {
	case st.Posture == "":
		postureStyle = render.Style{Fg: th.FgSubtle, Bg: bg}
	case st.Posture == flags.PostureYolo:
		postureStyle = render.Style{Fg: th.Danger, Bg: bg, Attrs: render.Bold}
	case flags.DescribePosture(st.Posture).Relaxed:
		postureStyle = strong
	}
	put(0, false, render.Styled(orDash(st.Posture), postureStyle))

	if st.TaskID != "" {
		put(2, false, render.Styled(st.TaskID, render.Style{Fg: th.Info, Bg: bg, Attrs: render.Bold}))
	}
	if st.Model != "" {
		put(3, false, render.Styled(st.Model, name))
	}

	// Right-hand cluster: what the session has spent and leaned on.
	if st.Busy && len(spark) > 2 {
		put(10, true, render.Sparkline(spark, render.Style{Fg: th.AccentDim, Bg: bg}))
	}
	if st.InputTokens > 0 || st.OutputTokens > 0 {
		put(5, true, render.Styled(compactCount(st.InputTokens)+"↓ "+compactCount(st.OutputTokens)+"↑", base))
	}
	if st.Grants > 0 {
		put(1, true, render.Styled(g.Granted+" "+itoa(st.Grants)+" granted",
			strong))
	}
	if st.Sessions > 1 {
		put(9, true, render.Styled(itoa(st.Sessions)+" sessions", base))
	}

	sep := render.Styled("  "+g.Dot+"  ", render.Style{Fg: th.FgSubtle, Bg: bg})
	join := func(right bool) render.Line {
		var out render.Line
		for i, s := range segs {
			if s.right != right {
				continue
			}
			switch {
			case len(out) == 0:
				if !right {
					out = out.Append(" ", base)
				}
			case right:
				out = out.Append("  ", base)
			case i > 0 && segs[i-1].rank == 0:
				out = out.Append("  ", base)
			default:
				out = out.Concat(sep)
			}
			out = out.Concat(s.line)
		}
		if right && len(out) > 0 {
			out = out.Append(" ", base)
		}
		return out
	}
	for {
		left, right := join(false), join(true)
		if left.Width()+right.Width()+1 <= r.W {
			left.Draw(b, r.X, r.Y)
			right.Draw(b, r.Right()-right.Width(), r.Y)
			return
		}
		drop := -1
		for i, s := range segs {
			if s.rank > 0 && (drop < 0 || s.rank > segs[drop].rank) {
				drop = i
			}
		}
		if drop < 0 {
			// Only the chip is left and the bar is narrower than it.
			left.Truncate(r.W).Draw(b, r.X, r.Y)
			return
		}
		segs = slices.Delete(segs, drop, drop+1)
	}
}

// DrawActivity paints the working row: what the turn is doing, for how long,
// and how to stop it.
//
// It sits directly above the composer. A turn in progress was a spinner on the
// status bar at the far edge of the screen from where the operator is typing
// and from where the reply is arriving, and the first thing a narrow bar
// dropped. fxOn gates the motion: the pulse on the label, the marquee on a
// label too long for the row, and the Fold M ribbon standing in for the
// spinner. The spinner itself is not gated. It is how a running turn is told
// from a hung one, so with motion off it is what is drawn.
func DrawActivity(b *render.Buffer, r render.Rect, th Theme, st Status, tick int, fxOn bool) {
	if r.Empty() || !st.Busy {
		return
	}
	g := th.Glyphs()
	b.Fill(r, ' ', th.Base())

	label := st.BusyLabel
	if label == "" {
		label = "working"
	}
	if st.StallSecs >= 15 {
		label = label + " (no progress " + itoa(st.StallSecs) + "s)"
	}
	// A turn that has gone quiet for half a minute is said in bold, not in a
	// colour: it may be a slow step, and only the count beside it says how slow.
	fg := th.Accent
	labelFg := th.Fg
	var labelAttrs render.Attr
	if st.StallSecs >= stallWarnSecs {
		labelAttrs = render.Bold
	}
	meta := "ctrl+c to interrupt"
	if st.BusySecs > 0 {
		meta = elapsed(st.BusySecs) + " " + g.Dot + " " + meta
	}
	meta = "  (" + meta + ")"
	// The lead is the spinner, or the Fold M ribbon where the ribbon can do the
	// spinner's job. The ribbon is wider than the gutter, so it takes the
	// spinner's place only where the row still fits whole with it, label and note
	// both: on a row already too short for those, the decoration is not worth the
	// room. This is decided on the label as it reads, before it is scrolled.
	lead := render.Styled(render.Spinner(g.Spinner, tick), render.Style{Fg: fg, Bg: th.Bg, Attrs: render.Bold})
	if pad := gutter - lead.Width(); pad > 0 {
		lead = lead.Append(strings.Repeat(" ", pad), th.Base())
	}
	if ribbon := busyRibbon(th, st, th.Bg, tick, fxOn); ribbon != nil &&
		ribbon.Width()+render.StringWidth(label)+render.StringWidth(meta) <= r.W {
		lead = ribbon
	}
	if fxOn {
		// The label breathes, and one that outgrows the row scrolls rather
		// than truncating — a phase like "waiting on the gate, retrying" is
		// the difference between a stall and a slow step. It has the row less
		// the lead and the note after it, and is not scrolled while it fits in that.
		labelFg = th.FgMuted.Blend(labelFg, 0.55+fx.Pulse(tick, 16)*0.45)
		label = fx.Marquee(label, max(r.W-lead.Width()-render.StringWidth(meta), 8), tick)
	}
	line := lead.Append(label, render.Style{Fg: labelFg, Bg: th.Bg, Attrs: labelAttrs})
	line = line.Append(meta, th.Subtle())
	line.Truncate(r.W).Draw(b, r.X, r.Y)
}

// elapsed writes a running time the way a person reads one off a clock: seconds
// alone under a minute, then minutes with the seconds padded so the row does
// not change width as it counts.
func elapsed(secs int) string {
	if secs < 60 {
		return itoa(secs) + "s"
	}
	s := itoa(secs % 60)
	if len(s) < 2 {
		s = "0" + s
	}
	if secs < 3600 {
		return itoa(secs/60) + "m " + s + "s"
	}
	m := itoa(secs / 60 % 60)
	if len(m) < 2 {
		m = "0" + m
	}
	return itoa(secs/3600) + "h " + m + "m"
}

// busyRibbon is the mark as the busy indicator: the one-line ribbon with light
// running round it, and the cell after it. It returns nil when the mark cannot
// do that job, and the caller keeps the spinner.
//
// A busy indicator has to move, and the ribbon's only motion is colour. So it
// is used only when it has a colour to move toward: animation on, a terminal
// that draws the blocks, and a theme that lends a glint, which the
// sixteen-colour one does not. That choice is made from the theme's own
// colours and not from a stall, so a stall cannot change it. The caller's width
// rule still can: a row that stops fitting whole part way through a turn,
// because the stall label appeared or a counter grew, goes back to the spinner.
//
// A turn that has gone quiet for half a minute is said in bold, not in a colour
// (see DrawActivity: the label goes bold), and the mark says the same thing the
// same way: it keeps its place and its colours and its cells go bold. The
// terminal UI has no warning colour left to turn it.
//
// bg is the background of the row the mark is set in.
func busyRibbon(th Theme, st Status, bg render.Color, tick int, fxOn bool) render.Line {
	faces := th.Logo()
	if !fxOn || !th.LogoBlocks() || faces.Glint.IsDefault() {
		return nil
	}
	// The ribbon's slanted blocks are half empty, so each face is drawn on the
	// background of the row it is set in, whatever the theme's own logo faces say.
	faces.Lit.Bg, faces.Shade.Bg = bg, bg
	mark := logo.Ribbon(faces, logo.Motion{Live: true, Tick: tick, Working: true})
	if st.StallSecs >= stallWarnSecs {
		for i := range mark {
			mark[i].Style.Attrs |= render.Bold
		}
	}
	return mark.Append(" ", render.Style{Bg: bg})
}

// DrawStrictBanner paints the persistent warning that this run's results are
// not strict, or nothing when they are.
//
// It occupies a row of its own rather than being folded into the status line.
// The distinction it carries — that a green result here was produced under
// relaxed settings — is exactly the one that gets lost when it is abbreviated.
func DrawStrictBanner(b *render.Buffer, r render.Rect, th Theme, st Status) {
	if r.Empty() {
		return
	}
	strict, why := st.StrictSummary()
	if strict {
		return
	}
	g := th.Glyphs()
	// In the warning colour on the screen's own background. It was a bar of
	// that colour from edge to edge, which a reader's eye went back to on every
	// frame of a session that had nothing new to say there; the row of its own
	// and the mark are what keep it from being missed.
	warn := th.Status(StatusWarn)
	b.Fill(r, ' ', th.Base())
	render.Styled(" "+g.Warn+" not a strict run: ", warn.With(render.Bold)).
		Append(why, warn).
		Truncate(r.W).Draw(b, r.X, r.Y)
}

// DrawShortcutBar paints the contextual key hints.
func DrawShortcutBar(b *render.Buffer, r render.Rect, th Theme, ctx Context, extra ...string) {
	if r.Empty() {
		return
	}
	line, fill := shortcutLine(th, ctx, r.W, extra...)
	b.Fill(r, ' ', fill)
	line.Truncate(r.W).Draw(b, r.X, r.Y)
}

// DrawFooter paints the one row under the composer: what the session is on the
// left, and the keys of the moment on the right.
//
// The posture is never given up: it is the one fact that changes what the rest
// of the screen means, so the keys are cut to leave it room. After it the keys
// and the notices beside them come first. They say what the operator can do
// next and what just changed — more approvals waiting, a transcript scrolled
// back — and the rest of the status gives up segments to make room for them,
// as it does on any narrow row.
func DrawFooter(b *render.Buffer, r render.Rect, th Theme, st Status, ctx Context, tick int, spark []float64, fxOn bool, extra ...string) {
	if r.Empty() {
		return
	}
	posture := 1 + render.StringWidth(orDash(st.Posture)) + 2
	// The keys leave room for the posture and the model. With the transcript
	// focused there are eight of them, and given the whole row they left the
	// status its posture alone, so the model came and went with the focus.
	// What does not fit is behind F1, as it always was. A notice outranks the
	// model: it says what just changed, and the model has not.
	keep := posture
	if st.Model != "" {
		keep += render.StringWidth(st.Model) + 2
	}
	budget := r.W - keep
	notices := 0
	for _, s := range extra {
		notices += 1 + render.StringWidth(s)
	}
	budget = min(max(budget, notices), r.W-posture)
	hints, fill := shortcutLine(th, ctx, budget, extra...)
	b.Fill(r, ' ', fill)
	if hints.Width() > budget {
		hints = hints.Truncate(max(budget, 0))
	}
	if r.W < sparkFrom {
		// The throughput sparkline is for a row with room to spare, measured
		// on the whole row and not on the status's share of it.
		spark = nil
	}
	room := r.W - hints.Width() - 1
	DrawStatusBar(b, render.Rect{X: r.X, Y: r.Y, W: room, H: 1}, th, st, tick, spark, fxOn)
	hints.Draw(b, r.Right()-hints.Width()-1, r.Y)
}

// sparkFrom is the narrowest footer that draws the throughput sparkline.
const sparkFrom = 110

// shortcutLine is the key hints for a context, cut to width, and the style the
// rest of their row is filled with.
func shortcutLine(th Theme, ctx Context, width int, extra ...string) (render.Line, render.Style) {
	r := render.Rect{W: width, H: 1}
	// The one row of key hints, and the quietest row on the screen: the key in
	// the secondary text colour, its label a step below, and space between
	// items instead of a rule. In the brand colour and bold, with a bar between
	// each, it was the loudest thing in the frame and said the least.
	bg := th.Bg
	keyStyle := render.Style{Fg: th.FgMuted, Bg: bg}
	labelStyle := render.Style{Fg: th.FgSubtle, Bg: bg}
	sep := render.Style{Fg: th.FgSubtle, Bg: bg}
	if th.monochrome() {
		// Nothing but weight tells a key from its label without colour.
		keyStyle.Attrs |= render.Bold
		labelStyle.Attrs |= render.Dim
	}
	line := render.Line{}
	for _, s := range extra {
		line = line.Append(" "+s, render.Style{Fg: th.FgMuted, Bg: bg, Attrs: render.Italic})
	}
	hints := hintsFor(ctx)
	for i, bd := range hints {
		if bd.Label == "" || len(bd.Keys) == 0 {
			continue
		}
		itemLine := render.Line{}
		if line.Width() > 0 {
			itemLine = itemLine.Append("   ", sep)
		} else {
			itemLine = itemLine.Append(" ", sep)
		}
		itemLine = itemLine.Append(bd.Keys[0], keyStyle).Append(" "+bd.Label, labelStyle)
		if line.Width()+itemLine.Width() > r.W-12 && i < len(hints)-1 {
			break
		}
		line = line.Concat(itemLine)
		if line.Width() > r.W {
			break
		}
	}
	// Always, not only when the row ran out: the row names the keys of the
	// moment and never all of them, so the way to the rest is part of it.
	if r.W > 25 {
		more := render.Styled("   ", sep).Append("F1", keyStyle).Append(" more", labelStyle)
		if line.Width()+more.Width() <= r.W {
			line = line.Concat(more)
		}
	}
	return line, labelStyle
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// compactCount renders a token count in a fixed handful of characters, so the
// bar's layout does not shift as a turn runs.
func compactCount(n int) string {
	switch {
	case n < 1000:
		return itoa(n)
	case n < 1000000:
		return itoa(n/1000) + "." + itoa((n%1000)/100) + "k"
	}
	return itoa(n/1000000) + "." + itoa((n%1000000)/100000) + "M"
}

// TabRef records where a tab was drawn for mouse hit testing.
type TabRef struct {
	SessionIndex int
	IsNewButton  bool
	Rect         render.Rect
}

// DrawSessionTabBar paints the top session tab strip.
func DrawSessionTabBar(b *render.Buffer, r render.Rect, th Theme, views []*AgentView, active int, tick int) []TabRef {
	if r.Empty() || len(views) == 0 {
		return nil
	}
	g := th.Glyphs()
	bg := th.BgRaised
	b.Fill(r, ' ', render.Style{Fg: th.FgMuted, Bg: bg})

	var tabs []TabRef
	curX := r.X + 1
	drawnCount := 0
	hiddenApprovals := 0

	for i, v := range views {
		selected := i == active
		tabBg := bg
		fg := th.FgMuted
		if selected {
			tabBg = th.Selection
			fg = th.Fg
		}

		tabStyle := render.Style{Fg: fg, Bg: tabBg}
		if selected {
			tabStyle.Attrs |= render.Bold
		}

		// Status glyph for session state
		badge := g.Bullet + " "
		badgeStyle := render.Style{Fg: th.FgSubtle, Bg: tabBg}
		if v.Approval() != nil {
			badge = g.Warn + " "
			badgeStyle = render.Style{Fg: th.Fg, Bg: tabBg, Attrs: render.Bold}
		} else if v.Error() != "" {
			badge = "✕ "
			badgeStyle = render.Style{Fg: th.Danger, Bg: tabBg, Attrs: render.Bold}
		} else if v.Status.Busy {
			badge = render.Spinner(g.Spinner, tick) + " "
			badgeStyle = render.Style{Fg: th.Accent, Bg: tabBg, Attrs: render.Bold}
		} else if selected {
			badge = g.Caret + " "
			badgeStyle = render.Style{Fg: th.Accent, Bg: tabBg, Attrs: render.Bold}
		}

		title := itoa(i+1) + ":" + v.ID
		if v.Title != "" {
			title = itoa(i+1) + ":" + v.Title
		}
		tabLine := render.Styled(" ", tabStyle).
			Append(badge, badgeStyle).
			Append(title+" ", tabStyle)

		w := tabLine.Width()
		if curX+w >= r.Right()-18 {
			for j := i; j < len(views); j++ {
				if views[j].Approval() != nil {
					hiddenApprovals++
				}
			}
			break
		}

		tabRect := render.Rect{X: curX, Y: r.Y, W: w, H: 1}
		tabLine.Draw(b, curX, r.Y)
		tabs = append(tabs, TabRef{SessionIndex: i, Rect: tabRect})
		curX += w + 1
		drawnCount++
	}

	// Never drop overflow tabs silently: show +N overflow and surface any
	// off-screen session waiting on human approval.
	if hidden := len(views) - drawnCount; hidden > 0 && curX < r.Right()-6 {
		ov := render.Styled("+"+itoa(hidden), render.Style{Fg: th.FgMuted, Bg: bg})
		if hiddenApprovals > 0 {
			ov = ov.Append(" "+g.Warn+itoa(hiddenApprovals), render.Style{Fg: th.Danger, Bg: bg, Attrs: render.Bold})
		}
		ov.Truncate(r.Right()-curX-1).Draw(b, curX, r.Y)
		curX += ov.Width() + 1
	}

	// "+ New" tab button
	newW := 7
	if curX+newW < r.Right()-24 {
		newRect := render.Rect{X: curX, Y: r.Y, W: newW, H: 1}
		render.Styled(" + ", render.Style{Fg: th.Accent, Bg: bg, Attrs: render.Bold}).
			Append("New ", render.Style{Fg: th.FgSubtle, Bg: bg}).Draw(b, curX, r.Y)
		tabs = append(tabs, TabRef{IsNewButton: true, Rect: newRect})
	}

	// Right side hints, written as the shortcut bar writes its own.
	dot := "  " + g.Dot + "  "
	right := render.Styled("ctrl+t next"+dot+"ctrl+s switch"+dot+"ctrl+g all", render.Style{Fg: th.FgSubtle, Bg: bg})
	if r.W > 70 && r.Right()-right.Width()-1 > curX+newW {
		right.Draw(b, r.Right()-right.Width()-1, r.Y)
	}

	return tabs
}
