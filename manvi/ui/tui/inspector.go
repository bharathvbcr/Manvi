package tui

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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
	// collapsedAgents tracks which sub-agent sections are collapsed in TabSteps.
	collapsedAgents map[string]bool

	// Recorded geometry from the last Draw pass for mouse hit-testing.
	rect       render.Rect
	tabRects   [inspectorTabCount]render.Rect
	prevBtn    render.Rect
	nextBtn    render.Rect
	copyBtn    render.Rect
	closeBtn   render.Rect
	fileRects  []inspectorItemRect
	agentRects []inspectorAgentRect
	cache      inspectorCache
}

type inspectorItemRect struct {
	idx  int
	rect render.Rect
}

type inspectorAgentRect struct {
	agent string
	rect  render.Rect
}

type inspectorEdit struct {
	edit    fileEdit
	pending bool
	status  StatusKind
}

type inspectorCache struct {
	view        *AgentView
	entryCount  int
	dropped     int
	lastEntry   *Entry
	lastTextLen int
	lastLive    bool
	lastResult  *Entry
	thoughts    []*Entry
	edits       []inspectorEdit

	diffIdx     int
	diffWidth   int
	diffTheme   string
	diffUnicode bool
	diffLines   []render.Line
}

// NewInspector creates a closed inspector following the latest thought/diff.
func NewInspector() *Inspector {
	return &Inspector{
		thoughtIdx:      -1,
		diffIdx:         -1,
		collapsedAgents: map[string]bool{},
	}
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

// editsOf collects every succeeded file edit from the transcript.
// Calls with no result yet (pending approval/blocked) are excluded so callers
// never treat an unapplied edit as applied.
func editsOf(entries []*Entry) []fileEdit {
	var out []fileEdit
	for i, e := range entries {
		if e.Event.Kind != ui.KindToolStart || e.result == nil || e.result.Event.IsError {
			continue
		}
		if ed, ok := extractFileEdit(e.Event, i); ok {
			out = append(out, ed)
		}
	}
	return out
}

func inspectorEditsOf(entries []*Entry) []inspectorEdit {
	var out []inspectorEdit
	for i, e := range entries {
		if e.Event.Kind != ui.KindToolStart {
			continue
		}
		if e.result != nil && e.result.Event.IsError {
			continue
		}
		ed, ok := extractFileEdit(e.Event, i)
		if !ok {
			continue
		}
		ie := inspectorEdit{edit: ed}
		if e.result == nil {
			ie.pending = true
			ie.status = StatusWarn
		} else {
			ie.status = e.result.Status()
		}
		out = append(out, ie)
	}
	return out
}

func (ins *Inspector) cachedData(v *AgentView, entries []*Entry) ([]*Entry, []inspectorEdit) {
	var last *Entry
	var lastTextLen int
	var lastLive bool
	var lastResult *Entry
	if n := len(entries); n > 0 {
		last = entries[n-1]
		lastTextLen = last.Body.Len()
		lastLive = last.live
		lastResult = last.result
	}
	dropped := 0
	if v != nil && v.Scroll != nil {
		dropped = v.Scroll.Dropped()
	}
	c := &ins.cache
	if c.view == v &&
		c.entryCount == len(entries) &&
		c.dropped == dropped &&
		c.lastEntry == last &&
		c.lastTextLen == lastTextLen &&
		c.lastLive == lastLive &&
		c.lastResult == lastResult {
		return c.thoughts, c.edits
	}
	c.view = v
	c.entryCount = len(entries)
	c.dropped = dropped
	c.lastEntry = last
	c.lastTextLen = lastTextLen
	c.lastLive = lastLive
	c.lastResult = lastResult
	c.thoughts = thoughtsOf(entries)
	c.edits = inspectorEditsOf(entries)
	c.diffWidth = -1
	c.diffLines = nil
	return c.thoughts, c.edits
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
		eds := inspectorEditsOf(entries)
		if len(eds) == 0 {
			return "", ""
		}
		idx := ins.diffIdx
		if idx < 0 || idx >= len(eds) {
			idx = len(eds) - 1
		}
		ie := eds[idx]
		prefix := "diff "
		if ie.pending {
			prefix = "proposed diff "
		}
		return prefix + sanitizeHeaderPath(ie.edit.Path), formatUnifiedDiffText(ie.edit)
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

// ToggleAgent collapses or expands a sub-agent section in the Steps tab.
func (ins *Inspector) ToggleAgent(agent string) {
	if agent == "" {
		return
	}
	if ins.collapsedAgents == nil {
		ins.collapsedAgents = map[string]bool{}
	}
	ins.collapsedAgents[agent] = !ins.collapsedAgents[agent]
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
	for _, ar := range ins.agentRects {
		if ar.rect.Contains(x, y) {
			ins.ToggleAgent(ar.agent)
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
	ins.agentRects = ins.agentRects[:0]
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
	thoughts, edits := ins.cachedData(v, entries)

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

	agentLines := make(map[int]string, len(subOrder))
	if len(subOrder) > 0 {
		lines = append(lines, render.Line{})
		lines = append(lines, render.Styled("Subagents", th.Strong()))
		g := th.Glyphs()
		for _, ag := range subOrder {
			list := subagents[ag]
			collapsed := ins.collapsedAgents != nil && ins.collapsedAgents[ag]
			chev := g.Expanded
			if collapsed {
				chev = g.Collapsed
			}
			var blocks, grants, degraded, warns int
			var pinned []*Entry
			for _, ce := range list {
				st := ce.Status()
				if isSafetyPinnedStatus(st) {
					pinned = append(pinned, ce)
				}
				switch st {
				case StatusBlock:
					blocks++
				case StatusGranted:
					grants++
				case StatusDegraded:
					degraded++
				case StatusWarn:
					warns++
				}
			}
			hdr := render.Styled(chev+" ["+ag+"] ", th.Status(StatusInfo)).
				Append(fmt.Sprintf("%d events", len(list)), th.Subtle())
			if blocks > 0 {
				hdr = hdr.Append(fmt.Sprintf(" · %d failed", blocks), th.Status(StatusBlock))
			}
			if grants > 0 {
				hdr = hdr.Append(fmt.Sprintf(" · %d granted", grants), th.Status(StatusGranted))
			}
			if degraded > 0 {
				hdr = hdr.Append(fmt.Sprintf(" · %d unrun", degraded), th.Status(StatusDegraded))
			}
			if warns > 0 {
				hdr = hdr.Append(fmt.Sprintf(" · %d warn", warns), th.Status(StatusWarn))
			}
			agentLines[len(lines)] = ag
			lines = append(lines, hdr.Truncate(r.W))
			visible := list
			if collapsed {
				// Policy-honesty invariant: a collapsed sub-agent section must
				// never hide a block, failure, grant, or degraded row.
				visible = pinned
			}
			for _, ce := range visible {
				sub := ce.Summary(th, r.W-4)
				lines = append(lines, render.Styled("  "+ce.marker(g)+" ", th.Status(ce.Status())).Concat(sub).Truncate(r.W))
			}
		}
	}

	if len(lines) == 0 {
		render.Styled("No tool steps recorded yet.", th.Subtle()).Truncate(r.W).Draw(b, r.X, r.Y+1)
		return
	}
	start := ins.drawLines(b, r, lines, ins.scroll)
	for row := 0; row < r.H && start+row < len(lines); row++ {
		if ag, ok := agentLines[start+row]; ok {
			ins.agentRects = append(ins.agentRects, inspectorAgentRect{
				agent: ag,
				rect:  render.Rect{X: r.X, Y: r.Y + row, W: r.W, H: 1},
			})
		}
	}
}

func hasControlOrInvalidUTF8(s string) bool {
	if !utf8.ValidString(s) {
		return true
	}
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func sanitizeHeaderPath(s string) string {
	clean := ui.Sanitize(s)
	clean = strings.ReplaceAll(clean, "\n", " ")
	clean = strings.ReplaceAll(clean, "\r", " ")
	clean = strings.ReplaceAll(clean, "\t", " ")
	return clean
}

func fileLinkURL(path string) string {
	if path == "" || hasControlOrInvalidUTF8(path) {
		return ""
	}
	if strings.HasPrefix(path, "file://") {
		return path
	}
	abs := path
	if !filepath.IsAbs(path) {
		if resolved, err := filepath.Abs(path); err == nil {
			abs = resolved
		}
	}
	if hasControlOrInvalidUTF8(abs) {
		return ""
	}
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Clean(abs))}).String()
}

func (ins *Inspector) drawDiffsTab(b *render.Buffer, r render.Rect, th Theme, edits []inspectorEdit) {
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
	g := th.Glyphs()
	y := r.Y
	for k := startIdx; k < startIdx+listMax && k < len(edits) && y < r.Bottom(); k++ {
		ie := edits[k]
		ed := ie.edit
		sel := k == idx
		bg := th.Bg
		if sel {
			bg = th.Selection
			b.Fill(render.Rect{X: r.X, Y: y, W: r.W, H: 1}, ' ', render.Style{Bg: bg})
		}
		caret := "  "
		if sel {
			caret = g.Caret + " "
		}
		var line render.Line
		cleanPath := sanitizeHeaderPath(ed.Path)
		linkURL := fileLinkURL(ed.Path)
		pathStyle := render.Style{Fg: th.Info, Bg: bg}
		switch {
		case ie.pending:
			warnSt := th.Status(StatusWarn).With(render.Bold).Background(bg)
			line = render.Styled(caret+g.Tool+" ", warnSt).
				AppendLink(cleanPath, linkURL, pathStyle).
				Append(" (pending)", warnSt)
		case ie.status == StatusGranted || ie.status == StatusDegraded || ie.status == StatusWarn:
			st := th.Status(ie.status)
			st.Bg = bg
			glyph := g.Granted
			tag := " [granted]"
			switch ie.status {
			case StatusDegraded:
				glyph = g.Degraded
				tag = " [unrun]"
			case StatusWarn:
				glyph = g.Warn
				tag = " [warn]"
			}
			line = render.Styled(caret+glyph+" M ", st).
				AppendLink(cleanPath, linkURL, pathStyle).
				Append(tag, st)
		default:
			line = render.Styled(caret+"M ", render.Style{Fg: th.Accent, Bg: bg}).
				AppendLink(cleanPath, linkURL, pathStyle)
		}
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
	if edits[idx].edit.Tool == "patch_file" {
		render.Styled("snippet lines", th.Subtle()).Draw(b, r.X, y)
	}
	copyLbl := "[y] copy diff"
	cw := render.StringWidth(copyLbl)
	if r.W > cw {
		render.Styled(copyLbl, th.Subtle()).Draw(b, r.Right()-cw, y)
		ins.copyBtn = render.Rect{X: r.Right() - cw, Y: y, W: cw, H: 1}
	}
	y++

	diffRect := render.Rect{X: r.X, Y: y, W: r.W, H: r.Bottom() - y}
	diffLines := ins.cachedDiffLines(edits[idx], idx, th, diffRect.W)
	ins.drawLines(b, diffRect, diffLines, ins.scroll)
}

func (ins *Inspector) cachedDiffLines(ie inspectorEdit, idx int, th Theme, width int) []render.Line {
	c := &ins.cache
	if c.diffIdx == idx && c.diffWidth == width && c.diffTheme == th.Name && c.diffUnicode == th.Unicode && c.diffLines != nil {
		return c.diffLines
	}
	ed := ie.edit
	ed.Path = sanitizeHeaderPath(ed.Path)
	var lines []render.Line
	if ie.pending {
		lines = append(lines, render.Styled("Pending approval — proposed diff (not yet applied)", th.Status(StatusWarn)))
	}
	lines = append(lines, renderEditDiff(ed, th, width, 0)...)
	c.diffIdx = idx
	c.diffWidth = width
	c.diffTheme = th.Name
	c.diffUnicode = th.Unicode
	c.diffLines = lines
	return lines
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

func (ins *Inspector) drawLines(b *render.Buffer, r render.Rect, lines []render.Line, start int) int {
	if r.Empty() || len(lines) == 0 {
		return 0
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
	return start
}
