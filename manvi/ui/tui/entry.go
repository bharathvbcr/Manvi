package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bharathvbcr/DevCouncil/backend/go_orchestrator/flags"
	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/logo"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
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

	// stream keeps a growing reply's finished rows, so a delta re-renders the
	// line it extended rather than the whole reply. Two slots, because a frame
	// over a transcript shorter than the pane lays the entry out at two widths;
	// see streamRows.
	stream     [2]streamSlot
	streamLast int

	// lower is the text lowercased for search, and lowerOf the text it was
	// made from. Comparing against the text rather than clearing on every
	// delta keeps the copy correct whatever changed the text; the comparison
	// costs nothing in the usual case, as an unchanged body is the same
	// string and a grown one is a longer one.
	lowerOf, lower string

	// whole renders a reply in one pass, without the stream slots. Only tests
	// set it: it is the reference the slots are checked against, and a
	// reference that went through the slots' code would share their bugs.
	whole bool
}

// streamSlot is a reply's finished rows at one body width and theme.
type streamSlot struct {
	body  int
	theme string
	used  bool
	// src is the prefix of the text that rows describes. It ends just past a
	// line break, or is empty: text after the last break is the line still
	// arriving, and is never cached.
	src string
	// rows are src's rows in their final form, gutter and attribution
	// applied, so a delta copies them rather than rebuilding each. The marker
	// and attribution come from the event, which an entry never changes once
	// appended; only its body grows.
	rows []render.Line
	// md is the markdown state at the end of src.
	md mdState
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
	line = line.Append(summaryBody(e.text(), width), th.Muted())
	return line.Truncate(width)
}

// summaryBody is the folded entry's text: its first line, trimmed, tabs as
// spaces, marked when more follows.
//
// Only as much is built as a summary width columns wide can show. Built whole,
// a folded multi-megabyte result copied its entire first line — and for a
// single-line result that is all of it — to keep a few dozen columns. Cutting
// early is exact because a width is a sum over runes, so once a prefix is
// wider than the summary the rest cannot change what Truncate keeps; the one
// thing beyond the cut that can is trailing whitespace the trim would have
// removed, so the long form is kept whenever only whitespace follows.
func summaryBody(text string, width int) string {
	whole := func() string { return strings.ReplaceAll(strings.TrimSpace(firstLine(text)), "\t", " ") }
	line, more := text, false
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		line, more = text[:i], true
	}
	line = strings.TrimLeftFunc(line, unicode.IsSpace)
	used := 0
	for i, r := range line {
		if r == '\t' {
			r = ' '
		}
		if used += render.RuneWidth(r); used <= width {
			continue
		}
		end := i + utf8.RuneLen(r)
		if r == utf8.RuneError {
			_, end = utf8.DecodeRuneInString(line[i:])
			end += i
		}
		rest := line[end:]
		if !more && strings.IndexFunc(rest, func(r rune) bool { return !unicode.IsSpace(r) }) < 0 {
			return whole()
		}
		return strings.ReplaceAll(line[:end], "\t", " ")
	}
	return whole()
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
//
// The stream slots survive it. They are checked against the text itself on
// every use, and keeping them across a delta is their entire purpose.
func (e *Entry) Invalidate() { e.cacheLines = nil }

// settle releases the stream slots, for a reply that has stopped growing. They
// only pay for themselves while deltas arrive; afterwards they would hold a
// second index of the reply's rows, and a slot at a width the pane has left,
// for nothing — a later fold or resize renders the whole reply once either way.
func (e *Entry) settle() { e.stream = [2]streamSlot{} }

// lowerText is the text lowercased, as search compares it.
func (e *Entry) lowerText() string {
	if t := e.text(); t != e.lowerOf {
		e.lowerOf, e.lower = t, strings.ToLower(t)
	}
	return e.lower
}

const gutter = 3

// gutterPad indents a continuation row under the marker.
var gutterPad = strings.Repeat(" ", gutter)

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
	// ready counts the leading rows of out already in their final form, which
	// only a streamed reply's cached rows are.
	ready := 0
	add := func(l render.Line) { out = append(out, l) }
	addWrapped := func(text string, s render.Style) {
		for _, l := range render.WrapText(text, body, s) {
			add(l)
		}
	}
	// addCapped is addWrapped for text of unbounded length — an error carrying
	// a command's whole stderr, a harness message quoting a payload — under the
	// cap tool results and reasoning take; see capRendered.
	addCapped := func(text string, s render.Style) {
		shown, rest := capRendered(text)
		addWrapped(shown, s)
		if rest != "" {
			addWrapped(omittedMarker(rest), th.Subtle())
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
		addCapped("harness: "+ev.Text, th.Status(StatusWarn))

	case ui.KindText:
		if e.whole {
			out = renderMarkdown(e.text(), th, body, false)
		} else {
			out, ready = e.streamRows(th, body, g)
		}

	case ui.KindReasoning:
		// Capped as a tool result is, and for the same reason: a model's
		// thinking can run to megabytes, and unfolding it rendered every byte.
		addCapped(e.text(), th.Subtle().With(render.Italic))

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
		shown, rest := capRendered(e.text())
		for _, l := range renderMarkdown(shown, th, body, true) {
			add(l)
		}
		if rest != "" {
			addWrapped(omittedMarker(rest), th.Subtle())
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
		// The cost and the split by model, composed by whoever priced the
		// usage; see ui.ModelUsage.
		for _, line := range strings.Split(ev.Text, "\n") {
			if line != "" {
				addWrapped("  "+line, th.Subtle())
			}
		}

	case ui.KindReport:
		add(render.Styled(ev.Text, th.Strong()))

	case ui.KindError:
		addCapped(ev.Text, th.Status(StatusBlock))

	default:
		addCapped(e.text(), th.Muted())
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
		addWrapped("checks that did not run: "+ui.OneLine(strings.Join(ev.Degraded, ", ")),
			th.Status(StatusDegraded))
	}
	if len(ev.Weakened) > 0 {
		addWrapped("safety settings off default: "+ui.OneLine(strings.Join(ev.Weakened, ", ")),
			th.Status(StatusWarn))
	}

	if len(out) == 0 {
		out = append(out, render.Line{})
	}
	for i := ready; i < len(out); i++ {
		out[i] = e.finishRow(th, g, body, i, out[i])
	}
	return out
}

// finishRow puts row i of an entry's body into its final form.
func (e *Entry) finishRow(th Theme, g Glyphs, body, i int, l render.Line) render.Line {
	// Continuation rows are indented to line up under the marker.
	if i > 0 {
		return render.Styled(gutterPad, th.Base()).Concat(l)
	}
	// The same attribution the folded summary carries. Expanding a child's
	// entry is how a reader looks at what the child did, so the expanded form
	// is the last place it can be dropped.
	if e.Event.Agent != "" {
		l = render.Styled("["+e.Event.Agent+"] ", th.Subtle()).Concat(l).Truncate(body)
	}
	// The gutter marker goes on the first row.
	return e.gutterLine(th, l, g)
}

// streamRows renders an assistant reply as renderMarkdown would, reusing the
// rows of every line that has already ended.
//
// A reply arrives as hundreds of deltas and was re-rendered whole for each:
// split, highlighted and wrapped from its first byte, so a long reply cost the
// square of its length. Only the last line can still change. A line's rows are
// decided by that line and the fence state before it (see mdState), and text
// is only ever added after the last break, so every row of every finished line
// is final once its break arrives: an open backtick or an unpaired "**" on the
// last line is resolved by text on that same line or not at all, and a "#" or
// "-" that may yet become a heading or a list item is on the last line too. The
// rows from the last break onward are rendered fresh each time; the boundary is
// a break and never a point inside a line, since a line's inline spans and its
// wrapping can both change as it grows.
//
// The returned rows are final up to ready, unfinished after it.
func (e *Entry) streamRows(th Theme, body int, g Glyphs) (out []render.Line, ready int) {
	text := e.text()
	if text == "" {
		return nil, 0
	}
	sl := e.streamSlot(th.Name, body)
	// The rows describe src; text that does not still begin with it is not
	// the text they were made from. An unchanged prefix of a builder's buffer
	// is the same memory, which the comparison notices before reading a byte.
	if !strings.HasPrefix(text, sl.src) {
		*sl = streamSlot{body: body, theme: th.Name, used: true}
	}
	if done := strings.LastIndexByte(text, '\n') + 1; done > len(sl.src) {
		// Split(a+"\n"+b) is Split(a) followed by Split(b), which is why
		// rendering the newly finished lines on from the cached state yields
		// the rows a render of the whole text would.
		var rows []render.Line
		for _, raw := range strings.Split(text[len(sl.src):done-1], "\n") {
			rows = sl.md.line(rows, raw, th, body)
		}
		for _, l := range rows {
			sl.rows = append(sl.rows, e.finishRow(th, g, body, len(sl.rows), l))
		}
		sl.src = text[:done]
	}
	// A copy, because build appends to what it returns and the entry's cache
	// keeps it; sl.rows grows in place on the next delta.
	out = make([]render.Line, len(sl.rows), len(sl.rows)+4)
	copy(out, sl.rows)
	md := sl.md
	return md.line(out, text[len(sl.src):], th, body), len(sl.rows)
}

// streamSlot finds the stream slot for a body width and theme, replacing the
// one not used last when neither matches.
func (e *Entry) streamSlot(theme string, body int) *streamSlot {
	for i := range e.stream {
		if sl := &e.stream[i]; sl.used && sl.body == body && sl.theme == theme {
			e.streamLast = i
			return sl
		}
	}
	i := 1 - e.streamLast
	e.stream[i] = streamSlot{body: body, theme: theme, used: true}
	e.streamLast = i
	return &e.stream[i]
}

// The share of a tool result, or of a model's reasoning, the transcript
// renders.
//
// A read_file of a large file or a chatty exec hands back megabytes, and every
// byte of it used to be split, highlighted and wrapped — tens of thousands of
// rows, rebuilt again on each resize, fold and theme switch, for a result
// nobody scrolls through in a terminal pane. The cut is applied to what is
// drawn and nothing else: the entry keeps the whole text, so search still
// finds a match past the cut, and a copy is taken from the result rather than
// from the rows that happened to be drawn.
const (
	renderedMaxLines = 2000
	renderedMaxBytes = 64 << 10
)

// capRendered splits a tool result or reasoning into the part rendered and the
// part not. rest is empty when the whole text fits, and shown is then the text
// itself, so text under the cap renders exactly as it always did.
//
// The cut falls at a line break where there is one inside the budget, so the
// last row drawn is a whole line rather than half of one; only a single line
// longer than the entire budget is cut mid-line, and then on a rune boundary,
// never through a multi-byte character.
func capRendered(text string) (shown, rest string) {
	limit := len(text)
	if limit > renderedMaxBytes {
		limit = renderedMaxBytes
	}
	cut := -1
	for i, n := 0, 0; i < limit; i++ {
		j := strings.IndexByte(text[i:limit], '\n')
		if j < 0 {
			break
		}
		i += j
		if n++; n == renderedMaxLines {
			cut = i
			break
		}
	}
	if cut < 0 {
		if len(text) <= renderedMaxBytes {
			return text, ""
		}
		cut = strings.LastIndexByte(text[:limit], '\n')
	}
	if cut >= 0 {
		// A break that is the result's last byte leaves nothing behind it, and
		// a marker announcing one empty line would be noise.
		if cut == len(text)-1 {
			return text, ""
		}
		return text[:cut], text[cut+1:]
	}
	cut = limit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut], text[cut:]
}

// omittedMarker is the row that stands where the rest of a capped result
// would have been. It says how much is missing, because a cut that does not
// announce itself reads as the tool having said less than it did.
//
// It does not point at y. That copies the selected entry, but through OSC 52,
// which is bounded well below what a result this size can hold, and a result
// holding one fenced block has only that block copied — so "copy for the
// rest" would be a promise the key does not keep.
func omittedMarker(rest string) string {
	lines := strings.Count(rest, "\n")
	if !strings.HasSuffix(rest, "\n") {
		lines++
	}
	unit := " more lines"
	if lines == 1 {
		unit = " more line"
	}
	return "… " + groupDigits(lines) + unit + " (" + sizeLabel(len(rest)) + ") not rendered"
}

// groupDigits writes a count with thousands separators, so 12345 lines reads
// at a glance.
func groupDigits(n int) string {
	s := itoa(n)
	if n < 0 || len(s) <= 3 {
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	b.WriteString(s[:head])
	for i := head; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// sizeLabel renders a byte count in the unit a person would use for it.
func sizeLabel(n int) string {
	switch {
	case n < 1<<10:
		return itoa(n) + " B"
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
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
	var out []render.Line
	var st mdState
	for _, raw := range strings.Split(text, "\n") {
		out = st.line(out, raw, th, width)
	}
	return out
}

// mdState is everything renderMarkdown carries from one line to the next:
// whether a fence is open, and its language. Every other construct here is
// decided by its own line alone — nothing looks ahead to the next line or back
// past the previous break — and that is the property a growing reply's
// incremental render (Entry.streamRows) is built on. A construct that did look
// across lines, a table sized to its widest row or a list renumbered by its
// successors, would have to be carried here or end that reuse.
type mdState struct {
	inCode bool
	lang   string
}

// line renders one raw line, without its break, appending its rows to out.
func (st *mdState) line(out []render.Line, raw string, th Theme, width int) []render.Line {
	trimmed := strings.TrimSpace(raw)

	if strings.HasPrefix(trimmed, "```") {
		if !st.inCode {
			st.inCode = true
			st.lang = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			if st.lang == "" {
				st.lang = "code"
			}
			hdr := "┌─ " + st.lang + " "
			topLine := render.Styled(hdr, render.Style{Fg: th.BorderFocus, Bg: th.BgInset, Attrs: render.Bold})
			if topLine.Width() < width {
				topLine = topLine.Append(strings.Repeat("─", width-topLine.Width()), render.Style{Fg: th.Border, Bg: th.BgInset})
			}
			return append(out, topLine.Truncate(width))
		}
		st.inCode = false
		botLine := render.Styled("└"+strings.Repeat("─", width-1), render.Style{Fg: th.Border, Bg: th.BgInset})
		return append(out, botLine.Truncate(width))
	}

	if st.inCode {
		codeLine := highlightCodeLine(raw, th, st.lang)
		if codeLine.Width() < width {
			codeLine = codeLine.Pad(width, render.Style{Bg: th.BgInset})
		}
		return append(out, codeLine.Truncate(width))
	}

	if strings.HasPrefix(raw, "diff --git ") || strings.HasPrefix(raw, "index ") || strings.HasPrefix(raw, "--- ") || strings.HasPrefix(raw, "+++ ") {
		return append(out, render.Styled(raw, render.Style{Fg: th.FgMuted, Bg: th.BgInset, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "@@") {
		return append(out, render.Styled(raw, render.Style{Fg: th.Info, Bg: th.BgInset, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "+") && len(raw) > 1 && raw[1] != '+' {
		diffLine := render.Styled("+ ", render.Style{Fg: th.Success, Bg: th.BgInset, Attrs: render.Bold}).
			Append(raw[1:], render.Style{Fg: th.Success, Bg: th.BgInset})
		return append(out, diffLine.Truncate(width))
	}
	if strings.HasPrefix(raw, "-") && len(raw) > 1 && raw[1] != '-' {
		diffLine := render.Styled("- ", render.Style{Fg: th.Danger, Bg: th.BgInset, Attrs: render.Bold}).
			Append(raw[1:], render.Style{Fg: th.Danger, Bg: th.BgInset})
		return append(out, diffLine.Truncate(width))
	}

	if strings.HasPrefix(raw, "# ") {
		heading := strings.TrimPrefix(raw, "# ")
		return append(out, render.Styled("■ "+heading, render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "## ") {
		heading := strings.TrimPrefix(raw, "## ")
		return append(out, render.Styled("◆ "+heading, render.Style{Fg: th.Fg, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
	}
	if strings.HasPrefix(raw, "### ") {
		heading := strings.TrimPrefix(raw, "### ")
		return append(out, render.Styled("▸ "+heading, render.Style{Fg: th.FgMuted, Bg: th.Bg, Attrs: render.Bold}).Truncate(width))
	}

	if strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- ") {
		itemText := strings.TrimSpace(trimmed[2:])
		bullet := render.Styled("  "+th.Glyphs().Bullet+" ", th.Status(StatusInfo))
		itemLine := bullet.Concat(parseInlineSpans(itemText, th, th.Base()))
		return append(out, itemLine.Wrap(width)...)
	}

	return append(out, parseInlineSpans(raw, th, th.Base()).Wrap(width)...)
}
