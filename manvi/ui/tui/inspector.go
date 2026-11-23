package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// InspectorTab selects which view the right-hand Inspector Drawer displays.
type InspectorTab int

const (
	TabThink InspectorTab = iota
	TabSteps
	TabDiffs
	TabInfo
	inspectorTabCount
)

// inspectorMinWidth is the narrowest terminal width that splits side-by-side
// with the Inspector Drawer. Below this width, the drawer is hidden so the main
// conversation never gets cramped.
const inspectorMinWidth = 116

// Inspector is the toggleable right-hand Adaptive Workbench drawer (Ctrl+B).
//
// It gives power users a single dedicated place for:
//   - Live regenerating thinking (without wasting main transcript space)
//   - Current turn steps and nested sub-agent activity
//   - Synthesized diffs of files modified in this session
//   - DevCouncil posture, active lease countdown, and token/cost breakdown
type Inspector struct {
	Open bool
	Tab  InspectorTab

	// thoughtIdx selects which reasoning step to display in TabThink; -1 means
	// follow the latest thought automatically (regenerating in place).
	thoughtIdx int
	// diffIdx selects which edited file to preview in TabDiffs; -1 means latest.
	diffIdx int
	// scroll is the vertical row scroll offset inside the active tab body.
	scroll int

	// Recorded geometry from the last Draw pass for mouse hit-testing.
	rect      render.Rect
	tabRects  [inspectorTabCount]render.Rect
	prevBtn   render.Rect
	nextBtn   render.Rect
	copyBtn   render.Rect
	closeBtn  render.Rect
	fileRects []inspectorItemRect
}

type inspectorItemRect struct {
	idx  int
	rect render.Rect
}

// NewInspector creates a closed inspector following the latest thought/diff.
func NewInspector() *Inspector {
	return &Inspector{thoughtIdx: -1, diffIdx: -1}
}

// Toggle opens or closes the drawer.
func (ins *Inspector) Toggle() {
	ins.Open = !ins.Open
	ins.scroll = 0
}

// SetTab switches the active inspector tab and resets its scroll offset.
func (ins *Inspector) SetTab(tab InspectorTab) {
	if tab < 0 || tab >= inspectorTabCount {
		return
	}
	ins.Tab = tab
	ins.scroll = 0
}

// NextTab cycles to the next or previous tab.
func (ins *Inspector) NextTab(delta int) {
	n := int(inspectorTabCount)
	ins.Tab = InspectorTab((int(ins.Tab) + delta + n) % n)
	ins.scroll = 0
}

// ScrollBy scrolls the drawer body by delta rows.
func (ins *Inspector) ScrollBy(delta int) {
	ins.scroll += delta
	if ins.scroll < 0 {
		ins.scroll = 0
	}
}

// Contains reports whether (x, y) lies inside the last drawn drawer rect.
func (ins *Inspector) Contains(x, y int) bool {
	return ins != nil && ins.Open && !ins.rect.Empty() && ins.rect.Contains(x, y)
}

// StepThought moves between reasoning steps in TabThink.
func (ins *Inspector) StepThought(delta int, total int) {
	if total <= 0 {
		return
	}
	cur := ins.thoughtIdx
	if cur < 0 || cur >= total {
		cur = total - 1
	}
	cur += delta
	if cur < 0 {
		cur = 0
	}
	if cur >= total-1 {
		ins.thoughtIdx = -1 // back to following latest
	} else {
		ins.thoughtIdx = cur
	}
	ins.scroll = 0
}

// thoughtsOf collects every reasoning entry from the transcript.
func thoughtsOf(entries []*Entry) []*Entry {
	var out []*Entry
	for _, e := range entries {
		if e.Event.Kind == ui.KindReasoning && strings.TrimSpace(e.text()) != "" {
			out = append(out, e)
		}
	}
	return out
}

// editsOf collects every successful file edit from the transcript.
func editsOf(entries []*Entry) []fileEdit {
	var out []fileEdit
	for i, e := range entries {
		if e.Event.Kind != ui.KindToolStart {
			continue
		}
		if e.result != nil && e.result.Event.IsError {
			continue
		}
		if ed, ok := extractFileEdit(e.Event, i); ok {
			out = append(out, ed)
		}
	}
	return out
}

// CopyContent returns the copyable text for the inspector's current tab.
func (ins *Inspector) CopyContent(v *AgentView) (label, text string) {
	if v == nil {
		return "", ""
	}
	entries := v.Scroll.Entries()
	switch ins.Tab {
	case TabThink:
		ts := thoughtsOf(entries)
		if len(ts) == 0 {
			return "", ""
		}
		idx := ins.thoughtIdx
		if idx < 0 || idx >= len(ts) {
			idx = len(ts) - 1
		}
		return "thinking step", ts[idx].text()
	case TabDiffs:
		eds := editsOf(entries)
		if len(eds) == 0 {
			return "", ""
		}
		idx := ins.diffIdx
		if idx < 0 || idx >= len(eds) {
			idx = len(eds) - 1
		}
		return "diff " + eds[idx].Path, formatUnifiedDiffText(eds[idx])
	case TabSteps:
		var lines []string
		for _, e := range entries {
			if isWorkEntry(e.Event.Kind) && !e.absorbed {
				lines = append(lines, e.Summary(Dark(), 200).Text())
			}
		}
		return "turn steps", strings.Join(lines, "\n")
	case TabInfo:
		var b strings.Builder
		fmt.Fprintf(&b, "posture: %s\nmodel: %s\nbranch: %s\ntokens: %d in / %d out\n",
			orDash(v.Status.Posture), orDash(v.Status.Model), orDash(v.Status.GitBranch),
			v.Status.InputTokens, v.Status.OutputTokens)
		return "session info", b.String()
	}
	return "", ""
}

// Click handles a left click inside the drawer and returns any copy text if
// the copy button was pressed.
func (ins *Inspector) Click(x, y int, v *AgentView) (copiedLabel, copiedText string) {
	if !ins.Contains(x, y) {
		return "", ""
	}
	if !ins.closeBtn.Empty() && ins.closeBtn.Contains(x, y) {
		ins.Open = false
		return "", ""
	}
	for t := InspectorTab(0); t < inspectorTabCount; t++ {
		if ins.tabRects[t].Contains(x, y) {
			ins.SetTab(t)
			return "", ""
		}
	}
	if !ins.prevBtn.Empty() && ins.prevBtn.Contains(x, y) && v != nil {
		ins.StepThought(-1, len(thoughtsOf(v.Scroll.Entries())))
		return "", ""
	}
	if !ins.nextBtn.Empty() && ins.nextBtn.Contains(x, y) && v != nil {
		ins.StepThought(1, len(thoughtsOf(v.Scroll.Entries())))
		return "", ""
	}
	if !ins.copyBtn.Empty() && ins.copyBtn.Contains(x, y) {
		return ins.CopyContent(v)
	}
	for _, fr := range ins.fileRects {
		if fr.rect.Contains(x, y) {
			ins.diffIdx = fr.idx
			ins.scroll = 0
			return "", ""
		}
	}
	return "", ""
}

// Draw paints the Inspector Drawer inside r, including a thin left divider.
func (ins *Inspector) Draw(b *render.Buffer, r render.Rect, th Theme, v *AgentView, focused bool) {
	ins.rect = render.Rect{}
	ins.prevBtn = render.Rect{}
	ins.nextBtn = render.Rect{}
	ins.copyBtn = render.Rect{}
	ins.closeBtn = render.Rect{}
	ins.fileRects = ins.fileRects[:0]
	for i := range ins.tabRects {
		ins.tabRects[i] = render.Rect{}
	}
	if !ins.Open || r.Empty() || r.W < 24 || v == nil {
		return
	}
	ins.rect = r

	// Left vertical rule separating transcript from drawer.
	divider, pane := r.SplitLeft(1)
	divColor := th.ruleColor()
	if focused {
		divColor = th.Accent
	}
	divGlyph := "│"
	if !th.Unicode {
		divGlyph = "|"
	}
	divStyle := render.Style{Fg: divColor, Bg: th.Bg}
	for y := divider.Y; y < divider.Bottom(); y++ {
		b.SetString(divider.X, y, divGlyph, divStyle)
	}

	b.Fill(pane, ' ', th.Base())
	inner := pane.Pad(0, 1, 0, 1)
	if inner.Empty() {
		return
	}

	entries := v.Scroll.Entries()
	thoughts := thoughtsOf(entries)
	edits := editsOf(entries)

	// Tab strip at top of drawer.
	tabRow, body := inner.SplitTop(1)
	sepRow, body := body.SplitTop(1)

	labels := [inspectorTabCount]string{
		"Think",
		"Steps",
		fmt.Sprintf("Diffs(%d)", len(edits)),
		"Info",
	}
	if len(edits) == 0 {
		labels[TabDiffs] = "Diffs"
	}

	cx := tabRow.X
	for t := InspectorTab(0); t < inspectorTabCount; t++ {
		lbl := " " + labels[t] + " "
		w := render.StringWidth(lbl)
		if cx+w > tabRow.Right()-6 {
			break
		}
		st := th.Subtle()
		if t == ins.Tab {
			st = render.Style{Fg: th.Fg, Bg: th.Bg, Attrs: render.Bold | render.Underline}
			if !th.monochrome() {
				st.Fg = th.Accent
			}
		}
		render.Styled(lbl, st).Draw(b, cx, tabRow.Y)
		ins.tabRects[t] = render.Rect{X: cx, Y: tabRow.Y, W: w, H: 1}
		cx += w + 1
	}

	closeLbl := "[×]"
	if !th.Unicode {
		closeLbl = "[x]"
	}
	closeW := render.StringWidth(closeLbl)
	if tabRow.Right()-closeW >= cx {
		render.Styled(closeLbl, th.Subtle()).Draw(b, tabRow.Right()-closeW, tabRow.Y)
		ins.closeBtn = render.Rect{X: tabRow.Right() - closeW, Y: tabRow.Y, W: closeW, H: 1}
	}

	hBar := "─"
	if !th.Unicode {
		hBar = "-"
	}
	render.Styled(strings.Repeat(hBar, sepRow.W), render.Style{Fg: th.ruleColor(), Bg: th.Bg}).
		Draw(b, sepRow.X, sepRow.Y)

	if body.Empty() {
		return
	}

	switch ins.Tab {
	case TabThink:
		ins.drawThinkTab(b, body, th, thoughts)
	case TabSteps:
		ins.drawStepsTab(b, body, th, entries)
	case TabDiffs:
		ins.drawDiffsTab(b, body, th, edits)
	case TabInfo:
		ins.drawInfoTab(b, body, th, v, entries)
	}
}

func (ins *Inspector) drawThinkTab(b *render.Buffer, r render.Rect, th Theme, thoughts []*Entry) {
	if len(thoughts) == 0 {
		render.Styled("No thinking trace in this session yet.", th.Subtle()).
			Truncate(r.W).Draw(b, r.X, r.Y+1)
		render.Styled("Reasoning streams here automatically as turns run.", th.Subtle()).
			Truncate(r.W).Draw(b, r.X, r.Y+2)
		return
	}
	idx := ins.thoughtIdx
	if idx < 0 || idx >= len(thoughts) {
		idx = len(thoughts) - 1
	}
	cur := thoughts[idx]

	hdr, body := r.SplitTop(1)
	g := th.Glyphs()
	head := render.Styled(g.Thought+" ", th.AccentStyle()).
		Append(fmt.Sprintf("Step %d/%d", idx+1, len(thoughts)), th.Strong())
	if cur.live {
		head = head.Append(" · live", render.Style{Fg: th.Accent, Bg: th.Bg})
	} else if d := cur.until.Sub(cur.Event.At); !cur.Event.At.IsZero() && d >= time.Second {
		head = head.Append(" · "+shortDuration(d), th.Subtle())
	}
	if cur.Event.Agent != "" {
		head = head.Append(" ["+cur.Event.Agent+"]", th.Subtle())
	}

	// Controls on the right: [◀] [▶] [y]
	prevTxt, nextTxt, copyTxt := "[<]", "[>]", "[y]"
	if th.Unicode {
		prevTxt, nextTxt = "[◀]", "[▶]"
	}
	ctrlW := render.StringWidth(prevTxt) + 1 + render.StringWidth(nextTxt) + 1 + render.StringWidth(copyTxt)
	head.Truncate(max(hdr.W-ctrlW-1, 8)).Draw(b, hdr.X, hdr.Y)
	if hdr.W >= ctrlW+12 {
		rx := hdr.Right() - ctrlW
		pw := render.StringWidth(prevTxt)
		nw := render.StringWidth(nextTxt)
		cw := render.StringWidth(copyTxt)
		render.Styled(prevTxt, th.Subtle()).Draw(b, rx, hdr.Y)
		ins.prevBtn = render.Rect{X: rx, Y: hdr.Y, W: pw, H: 1}
		rx += pw + 1
		render.Styled(nextTxt, th.Subtle()).Draw(b, rx, hdr.Y)
		ins.nextBtn = render.Rect{X: rx, Y: hdr.Y, W: nw, H: 1}
		rx += nw + 1
		render.Styled(copyTxt, th.Muted()).Draw(b, rx, hdr.Y)
		ins.copyBtn = render.Rect{X: rx, Y: hdr.Y, W: cw, H: 1}
	}

	_, body = body.SplitTop(1)
	if body.Empty() {
		return
	}

	shown, rest := capRendered(cur.text())
	lines := render.WrapText(shown, body.W, th.Muted().With(render.Italic))
	if rest != "" {
		lines = append(lines, render.WrapText(omittedMarker(rest), body.W, th.Subtle())...)
	}
	// Follow bottom automatically when viewing a live thought unless scrolled.
	start := ins.scroll
	if cur.live && ins.thoughtIdx < 0 && len(lines) > body.H {
		start = len(lines) - body.H
	}
	ins.drawLines(b, body, lines, start)
}

func (ins *Inspector) drawStepsTab(b *render.Buffer, r render.Rect, th Theme, entries []*Entry) {
	// Show the latest turn's steps (from the last KindTurnStart onward), or all if none.
	turnStart := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Event.Kind == ui.KindTurnStart {
			turnStart = i + 1
			break
		}
	}

	var lines []render.Line
	subagents := map[string][]*Entry{}
	var subOrder []string

	for i := turnStart; i < len(entries); i++ {
		e := entries[i]
		if e.absorbed || !isWorkEntry(e.Event.Kind) {
			continue
		}
		if e.Event.Agent != "" {
			if _, ok := subagents[e.Event.Agent]; !ok {
				subOrder = append(subOrder, e.Event.Agent)
			}
			subagents[e.Event.Agent] = append(subagents[e.Event.Agent], e)
			continue
		}
		row := e.Summary(th, r.W-2)
		if e.Event.Kind == ui.KindToolResult {
			head := e.resultHead(th)
			if len(head) > 0 {
				row = e.outcomeRow(head, e.text(), th, r.W-2, th.Muted())
			}
		} else if e.Event.Kind == ui.KindToolStart {
			state := callRunning
			if e.result != nil {
				if e.result.Event.IsError {
					state = callFailed
				} else {
					state = callDone
				}
			}
			row = callLine(e.Event, th, state)
		}
		mark := e.marker(th.Glyphs())
		lines = append(lines, render.Styled(mark+" ", th.Status(e.Status())).Concat(row).Truncate(r.W))
	}

	if len(subOrder) > 0 {
		lines = append(lines, render.Line{})
		lines = append(lines, render.Styled("Subagents", th.Strong()))
		g := th.Glyphs()
		for _, ag := range subOrder {
			list := subagents[ag]
			hdr := render.Styled(g.Expanded+" ["+ag+"] ", th.Status(StatusInfo)).
				Append(fmt.Sprintf("%d events", len(list)), th.Subtle())
			lines = append(lines, hdr.Truncate(r.W))
			for _, ce := range list {
				sub := ce.Summary(th, r.W-4)
				lines = append(lines, render.Styled("  "+g.Bullet+" ", th.Subtle()).Concat(sub).Truncate(r.W))
			}
		}
	}

	if len(lines) == 0 {
		render.Styled("No tool steps recorded yet.", th.Subtle()).Truncate(r.W).Draw(b, r.X, r.Y+1)
		return
	}
	ins.drawLines(b, r, lines, ins.scroll)
}

func (ins *Inspector) drawDiffsTab(b *render.Buffer, r render.Rect, th Theme, edits []fileEdit) {
	if len(edits) == 0 {
		render.Styled("No file edits in this session yet.", th.Subtle()).Truncate(r.W).Draw(b, r.X, r.Y+1)
		return
	}
	idx := ins.diffIdx
	if idx < 0 || idx >= len(edits) {
		idx = len(edits) - 1
	}

	// File list header (up to 4 files).
	listMax := len(edits)
	if listMax > 4 {
		listMax = 4
	}
	startIdx := 0
	if idx >= listMax {
		startIdx = idx - listMax + 1
	}
	y := r.Y
	for k := startIdx; k < startIdx+listMax && k < len(edits) && y < r.Bottom(); k++ {
		ed := edits[k]
		sel := k == idx
		bg := th.Bg
		if sel {
			bg = th.Selection
			b.Fill(render.Rect{X: r.X, Y: y, W: r.W, H: 1}, ' ', render.Style{Bg: bg})
		}
		prefix := "  M "
		if sel {
			prefix = th.Glyphs().Caret + " M "
		}
		line := render.Styled(prefix, render.Style{Fg: th.Accent, Bg: bg}).
			Append(ed.Path, render.Style{Fg: th.Info, Bg: bg})
		if ed.Added > 0 {
			line = line.Append(" +"+groupDigits(ed.Added), render.Style{Fg: th.Success, Bg: bg})
		}
		if ed.Removed > 0 {
			line = line.Append(" -"+groupDigits(ed.Removed), render.Style{Fg: th.Danger, Bg: bg})
		}
		line.Truncate(r.W).Draw(b, r.X, y)
		ins.fileRects = append(ins.fileRects, inspectorItemRect{
			idx:  k,
			rect: render.Rect{X: r.X, Y: y, W: r.W, H: 1},
		})
		y++
	}

	if y+1 >= r.Bottom() {
		return
	}
	// Copy diff button row.
	copyLbl := "[y] copy diff"
	cw := render.StringWidth(copyLbl)
	if r.W > cw {
		render.Styled(copyLbl, th.Subtle()).Draw(b, r.Right()-cw, y)
		ins.copyBtn = render.Rect{X: r.Right() - cw, Y: y, W: cw, H: 1}
	}
	y++

	diffRect := render.Rect{X: r.X, Y: y, W: r.W, H: r.Bottom() - y}
	diffLines := renderEditDiff(edits[idx], th, diffRect.W, 0)
	ins.drawLines(b, diffRect, diffLines, ins.scroll)
}

func (ins *Inspector) drawInfoTab(b *render.Buffer, r render.Rect, th Theme, v *AgentView, entries []*Entry) {
	st := v.Status
	var lines []render.Line
	kv := func(k, val string, valStyle render.Style) {
		lines = append(lines, render.Styled(fmt.Sprintf("%-9s ", k), th.Subtle()).Append(val, valStyle))
	}
	kv("Posture", orDash(st.Posture), th.Strong())
	kv("Model", orDash(st.Model), th.Base())
	if st.Provider != "" {
		kv("Provider", st.Provider, th.Muted())
	}
	if st.GitBranch != "" {
		br := st.GitBranch
		if st.GitDirty > 0 {
			br += fmt.Sprintf(" (%d dirty)", st.GitDirty)
		}
		kv("Branch", br, th.Muted())
	}
	if st.TaskID != "" {
		task := st.TaskID
		if !st.LeaseExpiry.IsZero() {
			rem := time.Until(st.LeaseExpiry)
			if rem <= 0 {
				task += " (EXPIRED)"
			} else {
				task += " (" + shortDuration(rem) + " left)"
			}
		}
		kv("Lease", task, th.Status(StatusInfo))
	}
	kv("Tokens", fmt.Sprintf("%s in / %s out", groupDigits(st.InputTokens), groupDigits(st.OutputTokens)), th.Muted())
	if st.ContextMax > 0 {
		pct := (st.ContextUsed * 100) / st.ContextMax
		kv("Context", fmt.Sprintf("%d%% (%s / %s)", pct, compactCount(st.ContextUsed), compactCount(st.ContextMax)), th.Muted())
	}

	strict, why := st.StrictSummary()
	lines = append(lines, render.Line{})
	if strict {
		lines = append(lines, render.Styled("Strict run: all gates enforced", th.Muted()))
	} else {
		lines = append(lines, render.Styled("Non-strict: "+why, th.Status(StatusWarn)))
	}
	if st.Grants > 0 {
		lines = append(lines, render.Styled(fmt.Sprintf("Overrides: %d human grant(s)", st.Grants), th.Status(StatusGranted)))
	}
	if len(st.Degraded) > 0 {
		lines = append(lines, render.Styled("Unrun checks: "+strings.Join(st.Degraded, ", "), th.Status(StatusDegraded)))
	}

	// Latest usage cost breakdown from transcript if present.
	for i := len(entries) - 1; i >= 0; i-- {
		if entries[i].Event.Kind == ui.KindUsage && strings.TrimSpace(entries[i].Event.Text) != "" {
			lines = append(lines, render.Line{})
			lines = append(lines, render.Styled("Last Turn Usage", th.Strong()))
			for _, uline := range strings.Split(entries[i].Event.Text, "\n") {
				if trimmed := strings.TrimSpace(uline); trimmed != "" {
					lines = append(lines, render.WrapText(trimmed, r.W, th.Muted())...)
				}
			}
			break
		}
	}

	ins.drawLines(b, r, lines, ins.scroll)
}

func (ins *Inspector) drawLines(b *render.Buffer, r render.Rect, lines []render.Line, start int) {
	if r.Empty() || len(lines) == 0 {
		return
	}
	maxOff := len(lines) - r.H
	if maxOff < 0 {
		maxOff = 0
	}
	if start > maxOff {
		start = maxOff
		ins.scroll = maxOff
	}
	if start < 0 {
		start = 0
		ins.scroll = 0
	}
	for row := 0; row < r.H && start+row < len(lines); row++ {
		lines[start+row].Truncate(r.W).Draw(b, r.X, r.Y+row)
	}
}
