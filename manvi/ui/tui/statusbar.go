package tui

import (
	"strings"
	"time"

	"manvi/flags"
	"manvi/ui/fx"
	"manvi/ui/render"
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
	Posture  string
	Model    string
	Provider string
	TaskID   string
	// LeaseExpiry is when the current task's lease lapses. Shown as a countdown
	// because a lease that expires mid-turn is how two agents end up in one
	// working tree.
	LeaseExpiry time.Time

	InputTokens  int
	OutputTokens int
	Steps        int

	// ContextUsed and ContextMax track context-window pressure.
	ContextUsed int
	ContextMax  int

	// Git status context.
	GitBranch string
	GitAhead  int
	GitBehind int
	GitDirty  int

	// Weakened names safety flags moved off their defaults.
	Weakened []string
	// Degraded names checks that could not run in this session.
	Degraded []string
	// Grants counts overrides issued, so a run that leaned on them is legible
	// at a glance rather than only in the ledger.
	Grants int

	Busy       bool
	BusyLabel  string
	StallSecs  int
	Sessions   int
	Interrupts int
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

// DrawStatusBar paints the identity row.
//
// spark is the recent tokens-per-tick series, drawn as a sparkline while a
// turn runs so "the model is working" has a shape rather than just a spinner.
// fxOn gates the motion: the pulse on the busy label and the marquee on a
// label too long for its strip.
func DrawStatusBar(b *render.Buffer, r render.Rect, th Theme, st Status, tick int, spark []float64, fxOn bool) {
	if r.Empty() {
		return
	}
	g := th.Glyphs()
	bg := th.BgRaised
	base := render.Style{Fg: th.FgMuted, Bg: bg}
	strong := render.Style{Fg: th.Fg, Bg: bg, Attrs: render.Bold}
	b.Fill(r, ' ', base)

	line := render.Line{}

	// Posture is first and it is a chip, not a word, because it is the single
	// fact that changes what everything else on this bar means.
	// Three colours for three answers to "is this run enforcing?": success for
	// strict, warning for a posture that still records what it demoted, danger
	// for one that has stopped asking. yolo is not a louder dev — an operator
	// glancing at the bar has to be able to tell the two apart without reading.
	postureStyle := render.Style{Fg: th.FgOn, Bg: th.Success, Attrs: render.Bold}
	switch {
	case st.Posture == "":
		postureStyle.Bg = th.FgSubtle
	case st.Posture == flags.PostureYolo:
		postureStyle.Bg = th.Danger
	case flags.DescribePosture(st.Posture).Relaxed:
		postureStyle.Bg = th.Warning
	}
	line = line.Append(" "+orDash(st.Posture)+" ", postureStyle).Append(" ", base)

	if st.Busy {
		label := st.BusyLabel
		if label == "" {
			label = "working"
		}
		if st.StallSecs >= 15 {
			label = label + " (no progress " + itoa(st.StallSecs) + "s)"
		}
		busyFg := th.Accent
		if st.StallSecs >= 30 {
			busyFg = th.Warning
		}
		if fxOn {
			// The label breathes, and one that outgrows its strip scrolls
			// rather than truncating — a phase like "waiting on the gate,
			// retrying" is the difference between a stall and a slow step.
			busyFg = busyFg.Blend(th.Fg, fx.Pulse(tick, 16)*0.45)
			label = fx.Marquee(label, 32, tick)
		}
		line = line.Append(render.Spinner(g.Spinner, tick)+" ",
			render.Style{Fg: busyFg, Bg: bg, Attrs: render.Bold}).
			Append(label+" ", render.Style{Fg: busyFg, Bg: bg})
	} else {
		line = line.Append(g.Pass+" ready ", render.Style{Fg: th.FgSubtle, Bg: bg})
	}

	if st.Model != "" {
		line = line.Append(g.Bullet+" ", render.Style{Fg: th.Border, Bg: bg}).
			Append(st.Model, strong)
		if st.Provider != "" {
			line = line.Append("@"+st.Provider, base)
		}
		line = line.Append(" ", base)
	}
	if st.TaskID != "" {
		line = line.Append(g.Bullet+" ", render.Style{Fg: th.Border, Bg: bg}).
			Append(st.TaskID, render.Style{Fg: th.Info, Bg: bg, Attrs: render.Bold})
		if !st.LeaseExpiry.IsZero() {
			remain := time.Until(st.LeaseExpiry)
			leaseStyle := render.Style{Fg: th.FgSubtle, Bg: bg}
			switch {
			case remain <= 0:
				// An expired lease is not a cosmetic detail: the task is
				// takeable by another builder from this moment.
				leaseStyle = render.Style{Fg: th.Danger, Bg: bg, Attrs: render.Bold}
				line = line.Append(" lease EXPIRED", leaseStyle)
			case remain < 2*time.Minute:
				leaseStyle = render.Style{Fg: th.Warning, Bg: bg, Attrs: render.Bold}
				line = line.Append(" lease "+shortDuration(remain), leaseStyle)
			default:
				line = line.Append(" lease "+shortDuration(remain), leaseStyle)
			}
		}
		line = line.Append(" ", base)
	}

	if st.GitBranch != "" {
		gitLine := " " + g.Bullet + " " + st.GitBranch
		if st.GitAhead > 0 {
			gitLine += "↑" + itoa(st.GitAhead)
		}
		if st.GitBehind > 0 {
			gitLine += "↓" + itoa(st.GitBehind)
		}
		if st.GitDirty > 0 {
			gitLine += " ●" + itoa(st.GitDirty)
		}
		line = line.Append(gitLine+" ", render.Style{Fg: th.AccentDim, Bg: bg})
	}

	// Right-hand cluster: counters, context pressure gauge, and throughput sparkline.
	right := render.Line{}
	if st.Busy && len(spark) > 2 && r.W >= 110 {
		right = render.Sparkline(spark, render.Style{Fg: th.AccentDim, Bg: bg}).
			Append("  ", base).
			Concat(right)
	}
	if st.ContextMax > 0 && st.ContextUsed > 0 {
		pct := (st.ContextUsed * 100) / st.ContextMax
		if pct > 100 {
			pct = 100
		}
		filled := (pct * 5) / 100
		blocks := ""
		for i := 0; i < 5; i++ {
			if i < filled {
				blocks += "▓"
			} else {
				blocks += "░"
			}
		}
		if !th.Unicode {
			blocks = strings.Repeat("#", filled) + strings.Repeat("-", 5-filled)
		}
		gaugeStyle := render.Style{Fg: th.Success, Bg: bg}
		if pct >= 90 {
			gaugeStyle = render.Style{Fg: th.Danger, Bg: bg, Attrs: render.Bold}
		} else if pct >= 70 {
			gaugeStyle = render.Style{Fg: th.Warning, Bg: bg}
		}
		right = right.Append("ctx "+blocks+" "+itoa(pct)+"%  ", gaugeStyle)
	}
	if st.Steps > 0 {
		right = right.Append("step "+itoa(st.Steps)+"  ", base)
	}
	if st.InputTokens > 0 || st.OutputTokens > 0 {
		right = right.Append(compactCount(st.InputTokens)+"↓ "+compactCount(st.OutputTokens)+"↑  ", base)
	}
	if st.Grants > 0 {
		right = right.Append(g.Granted+" "+itoa(st.Grants)+" granted  ",
			render.Style{Fg: th.Granted, Bg: bg, Attrs: render.Bold})
	}
	if st.Sessions > 1 {
		right = right.Append(itoa(st.Sessions)+" sessions ", base)
	}

	line.Truncate(r.W-right.Width()-1).Draw(b, r.X, r.Y)
	right.Draw(b, r.Right()-right.Width(), r.Y)
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
	style := render.Style{Fg: th.FgOn, Bg: th.Warning, Attrs: render.Bold}
	b.Fill(r, ' ', style)
	render.Styled(" "+g.Warn+" not a strict run: ", style).
		Append(why, render.Style{Fg: th.FgOn, Bg: th.Warning}).
		Truncate(r.W).Draw(b, r.X, r.Y)
}

// DrawShortcutBar paints the contextual key hints.
func DrawShortcutBar(b *render.Buffer, r render.Rect, th Theme, ctx Context, extra ...string) {
	if r.Empty() {
		return
	}
	bg := th.Bg
	keyStyle := render.Style{Fg: th.Accent, Bg: bg, Attrs: render.Bold}
	labelStyle := render.Style{Fg: th.FgSubtle, Bg: bg}
	sep := render.Style{Fg: th.Border, Bg: bg}
	b.Fill(r, ' ', labelStyle)

	line := render.Line{}
	for _, s := range extra {
		line = line.Append(" "+s, render.Style{Fg: th.FgMuted, Bg: bg, Attrs: render.Italic})
	}
	truncated := false
	hints := hintsFor(ctx)
	for i, bd := range hints {
		if bd.Label == "" || len(bd.Keys) == 0 {
			continue
		}
		itemLine := render.Line{}
		if line.Width() > 0 {
			itemLine = itemLine.Append("  "+th.Glyphs().VBar+" ", sep)
		}
		itemLine = itemLine.Append(bd.Keys[0], keyStyle).Append(" "+bd.Label, labelStyle)
		if line.Width()+itemLine.Width() > r.W-12 && i < len(hints)-1 {
			truncated = true
			break
		}
		line = line.Concat(itemLine)
		if line.Width() > r.W {
			truncated = true
			break
		}
	}
	if truncated && r.W > 25 {
		more := render.Styled(" … ", sep).Append("F1", keyStyle).Append(" more", labelStyle)
		if line.Width()+more.Width() <= r.W {
			line = line.Concat(more)
		}
	}
	line.Truncate(r.W).Draw(b, r.X, r.Y)
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
		badgeStyle := render.Style{Fg: th.Success, Bg: tabBg}
		if v.Approval() != nil {
			badge = g.Warn + " "
			badgeStyle = render.Style{Fg: th.Warning, Bg: tabBg, Attrs: render.Bold}
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
			break
		}

		tabRect := render.Rect{X: curX, Y: r.Y, W: w, H: 1}
		tabLine.Draw(b, curX, r.Y)
		tabs = append(tabs, TabRef{SessionIndex: i, Rect: tabRect})
		curX += w + 1
	}

	// "+ New" tab button
	newW := 7
	if curX+newW < r.Right()-24 {
		newRect := render.Rect{X: curX, Y: r.Y, W: newW, H: 1}
		render.Styled(" + ", render.Style{Fg: th.Accent, Bg: bg, Attrs: render.Bold}).
			Append("New ", render.Style{Fg: th.FgSubtle, Bg: bg}).Draw(b, curX, r.Y)
		tabs = append(tabs, TabRef{IsNewButton: true, Rect: newRect})
	}

	// Right side hints
	right := render.Styled("Ctrl+T Next • Ctrl+S Switch • Ctrl+G All", render.Style{Fg: th.FgSubtle, Bg: bg})
	if r.W > 70 && r.Right()-right.Width()-1 > curX+newW {
		right.Draw(b, r.Right()-right.Width()-1, r.Y)
	}

	return tabs
}
