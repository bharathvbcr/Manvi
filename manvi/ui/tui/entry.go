package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"manvi/flags"
	"manvi/ui"
	"manvi/ui/logo"
	"manvi/ui/render"
)

// Entry is one item in a scrollback: an event, plus the view state that belongs
// to the terminal rather than to the event — whether it is folded, and the
// wrapped lines cached for the width it was last drawn at.
type Entry struct {
	Event ui.Event
	// Body accumulates streamed assistant text, which arrives as many events
	// and is one entry.
	Body strings.Builder
	// Folded hides the entry's detail, keeping its summary line.
	Folded bool
	// Foldable is false for entries with nothing to hide.
	Foldable bool

	cacheWidth  int
	cacheFolded bool
	cacheTheme  string
	cacheLines  []render.Line
}

// text is the entry's body: streamed text if it accumulated any, otherwise the
// event's own.
func (e *Entry) text() string {
	if e.Body.Len() > 0 {
		return e.Body.String()
	}
	return e.Event.Text
}

// Status classifies the entry for colouring.
//
// The order of the checks is the whole point. A tool result that succeeded but
// was allowed by a grant is reported as granted, not as a pass, and one reached
// with a check that could not run is reported as degraded. Only an outcome with
// no qualification at all is allowed to be StatusPass — that is the rendering
// half of the invariant the policy layer enforces.
func (e *Entry) Status() StatusKind {
	ev := e.Event
	if len(ev.Degraded) > 0 {
		return StatusDegraded
	}
	if len(ev.Weakened) > 0 {
		return StatusWarn
	}
	if ev.GrantID != "" || ev.Demoted != "" {
		return StatusGranted
	}

	switch ev.Kind {
	case ui.KindError:
		return StatusBlock
	case ui.KindPolicy:
		if ev.Severity == "hard" {
			return StatusBlock
		}
		return StatusWarn
	case ui.KindApproval:
		return StatusWarn
	case ui.KindToolResult:
		if ev.IsError {
			return StatusBlock
		}
		return StatusPass
	case ui.KindLease:
		return StatusInfo
	case ui.KindNotice, ui.KindUsage, ui.KindReasoning:
		return StatusNeutral
	}
	return StatusNeutral
}

// marker is the gutter glyph.
func (e *Entry) marker(g Glyphs) string {
	switch e.Event.Kind {
	case ui.KindToolStart:
		return g.Tool
	case ui.KindLease:
		return g.Lease
	case ui.KindTurnStart, ui.KindHarnessMessage:
		return g.VBar
	case ui.KindText, ui.KindReasoning:
		return " "
	}
	switch e.Status() {
	case StatusPass:
		return g.Pass
	case StatusBlock:
		return g.Block
	case StatusWarn:
		return g.Warn
	case StatusGranted:
		return g.Granted
	case StatusDegraded:
		return g.Degraded
	}
	return g.Bullet
}

// Summary is the single line an entry collapses to.
func (e *Entry) Summary(th Theme, width int) render.Line {
	g := th.Glyphs()
	st := th.Status(e.Status())
	ev := e.Event

	var head string
	switch ev.Kind {
	case ui.KindToolStart:
		head = ev.Tool
	case ui.KindToolResult:
		head = "result"
	case ui.KindPolicy:
		head = ev.Rule
	case ui.KindText:
		head = "assistant"
	case ui.KindReasoning:
		head = "thinking"
	default:
		head = string(ev.Kind)
	}

	line := render.Line{}
	if e.Foldable {
		line = line.Append(g.Collapsed+" ", th.Subtle())
	}
	// A sub-agent's evidence is forwarded into this session's transcript, so
	// each such line has to say which child it came from. Set and rendered
	// nowhere was the condition ui.Event.Agent's own doc rules out: two agents'
	// evidence in one stream is only readable if each line says whose it is.
	if ev.Agent != "" {
		line = line.Append("["+ev.Agent+"] ", th.Subtle())
	}
	line = line.Append(head+" ", st)
	body := strings.ReplaceAll(strings.TrimSpace(firstLine(e.text())), "\t", " ")
	line = line.Append(body, th.Muted())
	return line.Truncate(width)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

// Lines renders the entry to a width, caching the result.
//
// The cache is keyed on everything that changes the output. Keying it on width
// alone is the obvious version and it is wrong: folding an entry or switching
// theme leaves the stale lines on screen, and the bug looks like a rendering
// glitch rather than a cache miss.
func (e *Entry) Lines(th Theme, width int) []render.Line {
	if width <= 0 {
		return nil
	}
	if e.cacheLines != nil && e.cacheWidth == width &&
		e.cacheFolded == e.Folded && e.cacheTheme == th.Name {
		return e.cacheLines
	}
	lines := e.build(th, width)
	e.cacheLines, e.cacheWidth, e.cacheFolded, e.cacheTheme = lines, width, e.Folded, th.Name
	return lines
}

// Invalidate drops the cached rendering, for an entry whose body grew.
func (e *Entry) Invalidate() { e.cacheLines = nil }

const gutter = 3

func (e *Entry) build(th Theme, width int) []render.Line {
	g := th.Glyphs()
	ev := e.Event
	body := width - gutter
	if body < 8 {
		body = 8
	}

	if e.Folded {
		return []render.Line{e.gutterLine(th, e.Summary(th, body), g)}
	}

	var out []render.Line
	add := func(l render.Line) { out = append(out, l) }
	addWrapped := func(text string, s render.Style) {
		for _, l := range render.WrapText(text, body, s) {
			add(l)
		}
	}

	switch ev.Kind {
	case ui.KindSessionStart:
		// The mark opens every session, at its one-line rung: the transcript
		// is the wrong place for a splash, but the wrong place for an
		// unbranded first line too.
		title := render.Styled(logo.Name, th.AccentStyle())
		if th.LogoBlocks() {
			title = logo.Lines(logo.SizeCompact, th.Logo(), true, "", body)[0]
		}
		title = title.Append("  posture ", th.Muted()).
			Append(ev.Posture, th.Strong()).
			Append("  model ", th.Muted()).
			Append(ev.Model, th.Strong())
		add(title)

		// Onboarding / quick-action pill bar. The three repository actions lead:
		// they are the repeated maintenance loop this bar exists to shorten.
		if body > 40 {
			guide := render.Styled("  Quick: ", th.Muted()).
				Append("/pull ", th.Status(StatusWarn)).
				Append("/push ", th.Status(StatusWarn)).
				Append("/issues ", th.Status(StatusInfo)).
				Append(" • Inspect: ", th.Muted()).
				Append("/doctor ", th.Status(StatusInfo)).
				Append("/tools ", th.Status(StatusInfo)).
				Append(" • ", th.Muted()).
				Append("Ctrl+P ", render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}).
				Append("palette", th.Subtle())
			add(guide)
		}

		if effect := flags.DescribePosture(ev.Posture); effect.Relaxed {
			// Restated every session rather than once. The posture changes what
			// a clean run means, and an operator who has not seen it will read
			// a green result as an enforced one.
			addWrapped(effect.Notice, th.Status(StatusWarn))
		}

	case ui.KindTurnStart:
		turnLine := render.Styled(ev.Text, th.Strong())
		if !ev.At.IsZero() {
			turnLine = turnLine.Append("  "+ev.At.Local().Format("15:04"), th.Subtle())
		}
		add(turnLine)

	case ui.KindHarnessMessage:
		// Prefixed rather than merely styled. Colour alone is not a label: it
		// is invisible in a copied transcript, and a copied transcript is
		// exactly where a harness message attributed to the operator does its
		// damage.
		addWrapped("harness: "+ev.Text, th.Status(StatusWarn))

	case ui.KindText:
		for _, l := range renderMarkdown(e.text(), th, body, false) {
			add(l)
		}

	case ui.KindReasoning:
		addWrapped(e.text(), th.Subtle().With(render.Italic))

	case ui.KindToolStart:
		head := render.Styled(ev.Tool, th.Status(StatusInfo).With(render.Bold))
		if detail := toolDetail(ev); detail != "" {
			head = head.Append("  "+detail, th.Subtle())
		}
		if !ev.At.IsZero() {
			head = head.Append("  "+ev.At.Local().Format("15:04"), th.Subtle())
		}
		add(head.Truncate(body))

	case ui.KindToolResult:
		for _, l := range renderMarkdown(e.text(), th, body, true) {
			add(l)
		}

	case ui.KindPolicy:
		add(render.Styled(ev.Text, th.Status(e.Status())))
		if ev.Rule != "" {
			add(render.Styled("rule "+ev.Rule+"  severity "+ev.Severity, th.Subtle()))
		}
		if ev.Severity == "hard" {
			add(render.Styled("this rule is never grantable; change the approach", th.Subtle()))
		}

	case ui.KindApproval:
		add(render.Styled(ev.Text, th.Status(StatusWarn).With(render.Bold)))
		// A question raised through this same seam has no blocked path. The row
		// is dropped rather than printed with an empty subject, which reads as a
		// rule that fired on nothing.
		if ev.Path != "" {
			add(render.Styled("rule "+ev.Rule+"   path "+ev.Path, th.Subtle()))
		}

	case ui.KindApprovalDone:
		add(render.Styled(g.Arrow+" "+ev.Text, th.Muted()))

	case ui.KindLease:
		add(render.Styled(ev.Text, th.Status(StatusInfo)))

	case ui.KindUsage:
		add(render.Styled(fmt.Sprintf("%d in / %d out tokens", ev.InputTokens, ev.OutputTokens), th.Subtle()))

	case ui.KindReport:
		add(render.Styled(ev.Text, th.Strong()))

	case ui.KindError:
		addWrapped(ev.Text, th.Status(StatusBlock))

	default:
		addWrapped(e.text(), th.Muted())
	}

	// The qualification block. It is appended for every kind rather than only
	// for policy events, because a tool result that a grant allowed carries the
	// same fields and must carry the same disclosure.
	if ev.Demoted != "" {
		addWrapped(fmt.Sprintf("%s would have blocked this — allowed by %s", ev.Rule, ev.Demoted),
			th.Status(StatusGranted))
	}
	if ev.GrantID != "" {
		addWrapped(fmt.Sprintf("cleared by %s (%s) — not a clean pass", ev.GrantID, ev.GrantedBy),
			th.Status(StatusGranted))
	}
	if len(ev.Degraded) > 0 {
		// Printing nothing here is what would make an unexamined result look
		// examined, so it is never folded away and never truncated to a count.
		addWrapped("checks that did not run: "+strings.Join(ev.Degraded, ", "),
			th.Status(StatusDegraded))
	}
	if len(ev.Weakened) > 0 {
		addWrapped("safety settings off default: "+strings.Join(ev.Weakened, ", "),
			th.Status(StatusWarn))
	}

	if len(out) == 0 {
		out = append(out, render.Line{})
	}
	// The gutter marker goes on the first row; continuation rows are indented
	// to line up under it.
	out[0] = e.gutterLine(th, out[0], g)
	for i := 1; i < len(out); i++ {
		out[i] = render.Styled(strings.Repeat(" ", gutter), th.Base()).Concat(out[i])
	}
	return out
}

// gutterLine prefixes a line with its status marker.
func (e *Entry) gutterLine(th Theme, l render.Line, g Glyphs) render.Line {
	marker := e.marker(g)
	st := th.Status(e.Status())
	pad := gutter - render.StringWidth(marker)
	if pad < 1 {
		pad = 1
	}
	return render.Styled(marker, st).
		Append(strings.Repeat(" ", pad), th.Base()).
		Concat(l)
}

// toolDetail renders a tool call's arguments as a compact one-liner.
//
// Arguments are model-authored, so they are formatted from parsed JSON rather
// than echoed. Echoing the raw string would put whatever the model wrote —
// including newlines and escape sequences — straight into the frame.
func toolDetail(ev ui.Event) string {
	if ev.Detail != "" {
		return ev.Detail
	}
	if len(ev.Arguments) == 0 {
		return ""
	}
	var fields map[string]any
	if err := json.Unmarshal(ev.Arguments, &fields); err != nil {
		return ""
	}
	// A stable order: map iteration is random, and a tool banner that reorders
	// its own arguments between frames reads as the call changing.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var parts []string
	for _, k := range keys {
		v := fields[k]
		s := ""
		switch t := v.(type) {
		case string:
			s = t
		case float64:
			s = fmt.Sprintf("%g", t)
		case bool:
			s = fmt.Sprintf("%t", t)
		default:
			continue
		}
		parts = append(parts, k+"="+render.TruncateWidth(oneLine(s), 48, "…"))
		if len(parts) == 3 {
			break
		}
	}
	return strings.Join(parts, " ")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	return strings.ReplaceAll(s, "\t", " ")
}

// Timestamp renders the event's clock time for the metadata column.
func (e *Entry) Timestamp() string {
	if e.Event.At.IsZero() {
		return ""
	}
	return e.Event.At.Local().Format("15:04:05")
}

// Age is how long ago the entry arrived.
func (e *Entry) Age(now time.Time) time.Duration {
	if e.Event.At.IsZero() {
		return 0
	}
	return now.Sub(e.Event.At)
}

var codeKeywords = map[string]bool{
	"func": true, "type": true, "struct": true, "interface": true, "package": true,
	"import": true, "return": true, "if": true, "else": true, "for": true,
	"range": true, "switch": true, "case": true, "break": true, "continue": true,
	"const": true, "var": true, "let": true, "mut": true, "fn": true,
	"impl": true, "pub": true, "use": true, "def": true, "class": true,
	"self": true, "async": true, "await": true, "select": true, "from": true,
	"where": true, "insert": true, "update": true, "delete": true, "match": true,
	"nil": true, "null": true, "true": true, "false": true, "go": true, "defer": true,
	"SELECT": true, "FROM": true, "WHERE": true, "INSERT": true, "UPDATE": true,
	"DELETE": true, "JOIN": true, "CREATE": true, "TABLE": true, "GROUP": true, "ORDER": true,
}

func parseInlineSpans(line string, th Theme, baseStyle render.Style) render.Line {
	if !strings.Contains(line, "`") && !strings.Contains(line, "**") {
		return render.Styled(line, baseStyle)
	}

	var res render.Line
	runes := []rune(line)
	i := 0
	cur := strings.Builder{}

	flush := func(s render.Style) {
		if cur.Len() > 0 {
			res = res.Append(cur.String(), s)
			cur.Reset()
		}
	}

	for i < len(runes) {
		if runes[i] == '`' {
			flush(baseStyle)
			j := i + 1
			for j < len(runes) && runes[j] != '`' {
				j++
			}
			if j < len(runes) {
				code := string(runes[i+1 : j])
				res = res.Append(code, th.Inset().Foreground(th.Accent))
				i = j + 1
				continue
			}
		} else if i+1 < len(runes) && runes[i] == '*' && runes[i+1] == '*' {
			flush(baseStyle)
			j := i + 2
			for j+1 < len(runes) && !(runes[j] == '*' && runes[j+1] == '*') {
				j++
			}
			if j+1 < len(runes) {
				boldText := string(runes[i+2 : j])
				res = res.Append(boldText, baseStyle.With(render.Bold))
				i = j + 2
				continue
			}
		}
		cur.WriteRune(runes[i])
		i++
	}
	flush(baseStyle)
	return res
}

func highlightCodeLine(raw string, th Theme, lang string) render.Line {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "--") || strings.HasPrefix(trimmed, "/*") {
		return render.Styled(raw, render.Style{Fg: th.FgSubtle, Bg: th.BgInset, Attrs: render.Italic})
	}

	if strings.Contains(raw, "\"") || strings.Contains(raw, "'") {
		var line render.Line
		inQuote := false
		var quoteChar rune
		var cur strings.Builder
		for _, r := range raw {
			if !inQuote && (r == '"' || r == '\'') {
				if cur.Len() > 0 {
					line = line.Append(cur.String(), render.Style{Fg: th.Fg, Bg: th.BgInset})
					cur.Reset()
				}
				inQuote = true
				quoteChar = r
				cur.WriteRune(r)
			} else if inQuote && r == quoteChar {
				cur.WriteRune(r)
				line = line.Append(cur.String(), render.Style{Fg: th.Success, Bg: th.BgInset})
				cur.Reset()
				inQuote = false
			} else {
				cur.WriteRune(r)
			}
		}
		if cur.Len() > 0 {
			style := render.Style{Fg: th.Fg, Bg: th.BgInset}
			if inQuote {
				style = render.Style{Fg: th.Success, Bg: th.BgInset}
			}
			line = line.Append(cur.String(), style)
		}
		return line
	}

	var line render.Line
	var cur strings.Builder
	flushWord := func() {
		if cur.Len() == 0 {
			return
		}
		w := cur.String()
		cur.Reset()
		if codeKeywords[w] {
			line = line.Append(w, render.Style{Fg: th.Accent, Bg: th.BgInset, Attrs: render.Bold})
		} else {
			line = line.Append(w, render.Style{Fg: th.Fg, Bg: th.BgInset})
		}
	}

	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			cur.WriteRune(r)
		} else {
			flushWord()
			line = line.Append(string(r), render.Style{Fg: th.FgMuted, Bg: th.BgInset})
		}
	}
	flushWord()
	return line
}

func renderMarkdown(text string, th Theme, width int, isToolResult bool) []render.Line {
	if text == "" {
		return nil
	}
	rawLines := strings.Split(text, "\n")
	var out []render.Line
	inCodeBlock := false
	codeLang := ""

	for _, raw := range rawLines {
		trimmed := strings.TrimSpace(raw)

		if strings.HasPrefix(trimmed, "```") {
			if !inCodeBlock {
				inCodeBlock = true
				codeLang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				if codeLang == "" {
					codeLang = "code"
				}
				hdr := "┌─ " + codeLang + " "
				topLine := render.Styled(hdr, render.Style{Fg: th.BorderFocus, Bg: th.BgInset, Attrs: render.Bold})
				if topLine.Width() < width {
					topLine = topLine.Append(strings.Repeat("─", width-topLine.Width()), render.Style{Fg: th.Border, Bg: th.BgInset})
				}
				out = append(out, topLine.Truncate(width))
			} else {
				inCodeBlock = false
				botLine := render.Styled("└"+strings.Repeat("─", width-1), render.Style{Fg: th.Border, Bg: th.BgInset})
				out = append(out, botLine.Truncate(width))
			}
			continue
		}

		if inCodeBlock {
			codeLine := highlightCodeLine(raw, th, codeLang)
			if codeLine.Width() < width {
				codeLine = codeLine.Pad(width, render.Style{Bg: th.BgInset})
			}
			out = append(out, codeLine.Truncate(width))
			continue
		}

		if strings.HasPrefix(raw, "diff --git ") || strings.HasPrefix(raw, "index ") || strings.HasPrefix(raw, "--- ") || strings.HasPrefix(raw, "+++ ") {
			out = append(out, render.Styled(raw, render.Style{Fg: th.FgMuted, Bg: th.BgInset, Attrs: render.Bold}).Truncate(width))
			continue
		}
		if strings.HasPrefix(raw, "@@") {
			out = append(out, render.Styled(raw, render.Style{Fg: th.Info, Bg: th.BgInset, Attrs: render.Bold}).Truncate(width))
			continue
		}
		if strings.HasPrefix(raw, "+") && len(raw) > 1 && raw[1] != '+' {
			diffLine := render.Styled("+ ", render.Style{Fg: th.Success, Bg: th.BgInset, Attrs: render.Bold}).
				Append(raw[1:], render.Style{Fg: th.Success, Bg: th.BgInset})
			out = append(out, diffLine.Truncate(width))
			continue
		}
		if strings.HasPrefix(raw, "-") && len(raw) > 1 && raw[1] != '-' {
			diffLine := render.Styled("- ", render.Style{Fg: th.Danger, Bg: th.BgInset, Attrs: render.Bold}).
				Append(raw[1:], render.Style{Fg: th.Danger, Bg: th.BgInset})
			out = append(out, diffLine.Truncate(width))
			continue
		}

		if strings.HasPrefix(raw, "# ") {
			heading := strings.TrimPrefix(raw, "# ")
			out = append(out, render.Styled("■ "+heading, render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
			continue
		}
		if strings.HasPrefix(raw, "## ") {
			heading := strings.TrimPrefix(raw, "## ")
			out = append(out, render.Styled("◆ "+heading, render.Style{Fg: th.Fg, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
			continue
		}
		if strings.HasPrefix(raw, "### ") {
			heading := strings.TrimPrefix(raw, "### ")
			out = append(out, render.Styled("▸ "+heading, render.Style{Fg: th.FgMuted, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
			continue
		}

		if strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") {
			itemText := strings.TrimSpace(trimmed[2:])
			bullet := render.Styled("  "+th.Glyphs().Bullet+" ", th.Status(StatusInfo))
			itemLine := bullet.Concat(parseInlineSpans(itemText, th, th.Base()))
			for _, wl := range itemLine.Wrap(width) {
				out = append(out, wl)
			}
			continue
		}

		styledLine := parseInlineSpans(raw, th, th.Base())
		for _, wl := range styledLine.Wrap(width) {
			out = append(out, wl)
		}
	}
	return out
}
