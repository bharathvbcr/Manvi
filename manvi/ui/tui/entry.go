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

	// live marks a thought still arriving. It is drawn as its last few rows
	// under a heading, and settle folds it to the heading alone.
	live bool
	// until is when the last piece of a streamed entry arrived, which with the
	// event's own time is how long a thought took.
	until time.Time
	// foldSet records that the operator chose this entry's fold state, so
	// settle does not undo an entry opened on purpose.
	foldSet bool

	// call is the tool call a result answers, when the result came straight
	// after it. The result's row then names the call, and the call's own row
	// is not drawn again above it.
	call *Entry
	// absorbed marks an entry that draws no rows because another row says what
	// it would: a call whose result took its row, or a result that only
	// repeats the decision above it or confirms the call above it.
	absorbed bool
	// result is the result a call was paired with, absorbed or not. A call that
	// keeps its own row is worded by how that came out.
	result *Entry
	// under marks a result whose call is still drawn above it, with a decision
	// on the call between them. It does not name the call a second time.
	under bool

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
	// open holds the finished rows of the line after src, for a line that is a
	// plain paragraph and long; see openLine.
	open openLine
}

// openLine is the rows of a reply's open line that can no longer change.
//
// A reply that is one long paragraph — a base64 blob, minified JSON, a table
// row — never reaches a line break, so the finished-line cache above holds
// nothing of it, and every frame wrapped the whole line from its first byte and
// put every row through finishRow: the square of its length over the stream.
// Wrapping is greedy, and a row's width is the sum of its runes', so a row
// that is followed by two more cannot change when text is added after them,
// and wrapping from the start of any row reproduces it and every row after.
// openLine keeps the rows before the last two, in their final form, and the
// byte where the rest begins; a frame wraps only from there.
//
// It exists only for a line that settled says is a paragraph for good. A
// marker arriving later, a width change (a slot is one width) or text that is
// not an extension of what the rows describe drops it, and the line is laid
// out whole, as it was before this existed.
type openLine struct {
	// base is the number of the slot's rows ahead of the line; the rows here are
	// numbered from it, which finishRow needs to tell the first row of the
	// reply from the others.
	base int
	// prefix is the text of the open line the rows describe. A frame's open
	// line must begin with it. An unchanged prefix of a builder's buffer is the
	// same memory, which the comparison notices before reading a byte.
	prefix string
	rows   []render.Line
}

const (
	// openKeep is how many of the last rows of the open line are laid out
	// again every frame rather than kept. A delta changes the last row, and
	// can move a word from it to a new one; the row before is the margin for a
	// width that was only provisional — a multi-byte rune cut between deltas is
	// counted as invalid bytes, one column each, until its last byte arrives.
	// Such a rune is at most three bytes in the open and so spans at most
	// two rows once a row is at least two columns wide (openMinWidth).
	openKeep     = 2
	openMinWidth = 4
)

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
	case ui.KindTurnStart:
		return g.Prompt
	case ui.KindHarnessMessage:
		return g.VBar
	case ui.KindReasoning:
		return g.Thought
	case ui.KindText, ui.KindSessionStart, ui.KindUsage:
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
		head = e.thoughtLabel()
	case ui.KindTurnStart:
		head = "you"
	default:
		head = string(ev.Kind)
	}

	line := render.Line{}
	if ev.Kind == ui.KindReasoning {
		// A finished thought is its heading and nothing else. The gutter mark
		// already says what the row is, and the first words of the thinking
		// beside it were a second line of reply nobody asked to read.
		if ev.Agent != "" {
			line = line.Append("["+ev.Agent+"] ", th.Subtle())
		}
		return line.Append(head, th.Muted()).Truncate(width)
	}
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
	line = line.Append(head+"  ", st)
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
	if width <= 0 || e.absorbed {
		// An absorbed call has no rows of its own: its result's row names it.
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
//
// A thought that has stopped is also folded to its heading, unless the operator
// has set its fold themselves. It reports whether that changed what the entry
// draws, so the caller can lay it out again.
func (e *Entry) settle() bool {
	e.stream = [2]streamSlot{}
	if !e.live {
		return false
	}
	e.live = false
	if !e.foldSet {
		e.Folded = true
	}
	e.Invalidate()
	return true
}

// thoughtLabel heads a model's thinking: that it is under way, or how long it
// took. The time comes from the events' own clocks, so it is the same on a
// resumed session as it was live, and is left out when they carry none.
func (e *Entry) thoughtLabel() string {
	if e.live {
		return "Thinking"
	}
	if d := e.until.Sub(e.Event.At); !e.Event.At.IsZero() && d >= time.Second {
		return "Thought for " + shortDuration(d)
	}
	return "Thought"
}

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

	if e.summarized() {
		// The summary, and under it whatever qualifies the outcome. A fold hides
		// detail; that a grant cleared this, or that a check did not run, is not
		// detail, and is said whether the entry is open or not.
		out := e.qualify([]render.Line{e.Summary(th, body)}, th, body)
		for i := range out {
			out[i] = e.finishRow(th, g, body, i, out[i])
		}
		return out
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
		title = title.Append("  "+orDash(ev.Posture), th.Muted()).
			Append("  "+g.Dot+"  ", th.Subtle()).
			Append(orDash(ev.Model), th.Muted())
		// The quick actions. The three repository actions lead: they are the
		// repeated maintenance loop this guide exists to shorten. In the text
		// colours — in the status colours it read as three warnings before the
		// session had done anything — and at the far edge of the title's row
		// where both fit, on a row of its own where they do not.
		quick := render.Styled("Quick: ", th.Subtle()).
			Append("/pull /push /issues /doctor /tools", th.Muted())
		guide := quick.Append("  "+g.Dot+"  ", th.Subtle()).
			Append("Ctrl+P", th.Muted()).
			Append(" palette", th.Subtle())
		if guide.Width() > body {
			// The commands outlast the pointer to the palette: the footer
			// carries that key on every frame.
			guide = quick
		}
		switch {
		case title.Width()+4+guide.Width() <= body:
			add(title.Pad(body-guide.Width(), th.Base()).Concat(guide))
		case body > 40:
			add(title.Truncate(body))
			add(guide.Truncate(body))
		default:
			add(title.Truncate(body))
		}

		if effect := flags.DescribePosture(ev.Posture); effect.Relaxed {
			// Restated every session rather than once. The posture changes what
			// a clean run means, and an operator who has not seen it will read
			// a green result as an enforced one.
			addWrapped(effect.Notice, th.Status(StatusWarn))
		}

	case ui.KindTurnStart:
		// What was asked, wrapped in full. It was one row cut at the pane's
		// edge, which showed the start of a long question and an ellipsis where
		// the rest of it was. It is set apart by its weight and the prompt mark
		// in front of it, on the same ground as everything else: a filled band
		// behind it was a box around the one thing on screen the operator
		// already knows.
		asked := render.Style{Fg: th.Fg, Bg: th.Bg, Attrs: render.Bold}
		shown, rest := capRendered(ev.Text)
		rows := render.WrapText(shown, body, asked)
		if n := len(rows); n > 0 {
			rows[n-1] = stamped(rows[n-1], e.clock(), body, th.Base(), th.Subtle())
		}
		for _, l := range rows {
			add(l)
		}
		if rest != "" {
			addWrapped(omittedMarker(rest), th.Subtle())
		}

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
		quiet := th.Subtle().With(render.Italic)
		if e.live {
			// Still arriving: the heading and the last few rows, so a long
			// thought scrolls in place instead of pushing the transcript away.
			add(render.Styled(e.thoughtLabel()+"…", th.Muted().With(render.Italic)))
			for _, l := range tailRows(e.text(), body, quiet, thoughtTail) {
				add(l)
			}
			break
		}
		add(render.Styled(e.thoughtLabel(), th.Muted()))
		// Capped as a tool result is, and for the same reason: a model's
		// thinking can run to megabytes, and unfolding it rendered every byte.
		addCapped(e.text(), quiet)

	case ui.KindToolStart:
		// A call still running. Once its result arrives this row is the
		// result's; see Entry.call. One that keeps its own row, because a
		// decision sits between it and its result, is in the past tense once
		// that result is in.
		switch {
		case e.result == nil:
			add(callLine(ev, th, callRunning).Truncate(body))
		case e.result.Event.IsError:
			add(failedCall(callLine(ev, th, callFailed), th, body, ""))
		default:
			add(callLine(ev, th, callDone).Truncate(body))
		}

	case ui.KindToolResult:
		// What a tool printed, drawn as it printed it and a step quieter than
		// the reply around it.
		// On a terminal of sixteen colours the secondary text colour is the
		// one dark grey, which is fine for a label and not for forty lines of
		// output; there a result keeps the text colour.
		plain := th.Muted()
		if th.reducedPalette() {
			plain = th.Base()
		}
		head := e.resultHead(th)
		text := e.text()
		// What a failed call printed is set in from the calls around it behind
		// a rule in the failure colour. Flush with them and in the text colour,
		// each line of it read as one more row of the transcript: a "FAIL" under
		// "Ran go test" was in the same column and the same white as "Ran".
		quote, inner := func(l render.Line) render.Line { return l }, body
		if ev.IsError {
			rule := render.Styled(g.VBar+" ", render.Style{Fg: th.Danger, Bg: th.Bg})
			if w := body - rule.Width(); w >= 8 {
				quote, inner = func(l render.Line) render.Line { return rule.Concat(l) }, w
			}
		}
		// One line of error is said on the call's row, as one line of output
		// is: "Editing a.go · target not found". More than that is the block.
		said := ""
		if ev.IsError && e.Folded && len(head) > 0 {
			if one := strings.TrimSpace(text); one != "" && !strings.Contains(one, "\n") && len(one) <= body*4 {
				said = expandTabs(one)
			}
		}
		oneRow := false
		if ev.IsError && len(head) > 0 {
			bare := head.Width()
			head = failedCall(head, th, body, said)
			oneRow = said != "" && head.Width() > bare && strings.HasSuffix(head.Text(), said)
		}
		switch {
		case e.Folded && !ev.IsError, strings.TrimSpace(text) == "":
			// A call that worked is one row: what was called and how much it
			// printed. The output is a keypress away, and a transcript that
			// showed the head of every file read had no room for the reply.
			add(e.outcomeRow(head, text, th, body, plain))
		case oneRow:
			add(head)
		case e.Folded:
			// One that failed keeps the head of what it printed: that is the
			// part of a turn a reader came for.
			if len(head) > 0 {
				add(head.Truncate(body))
			}
			rows, hidden := e.preview(th, inner, plain)
			for _, l := range rows {
				add(quote(l))
			}
			if hidden != "" {
				add(quote(render.Styled(hidden, th.Subtle())))
			}
		default:
			if len(head) > 0 {
				add(head.Truncate(body))
			}
			shown, rest := capRendered(text)
			if rest == "" {
				// The line break a command's output ends with is not a row.
				shown = strings.TrimRight(shown, "\n")
			}
			for _, l := range renderPre(shown, th, inner, plain) {
				add(quote(l))
			}
			if rest != "" {
				addWrapped(omittedMarker(rest), th.Subtle())
			}
		}

	case ui.KindPolicy:
		// The decision, and the rule that made it beside it where it fits.
		note := ev.Rule
		if ev.Rule != "" && ev.Severity != "" {
			note += " (" + ev.Severity + ")"
		}
		out = append(out, trailed(render.WrapText(ev.Text, body, th.Status(e.Status())), note, th, body)...)
		if ev.Severity == "hard" {
			addWrapped("this rule is never grantable; change the approach", th.Subtle())
		}

	case ui.KindApproval:
		// A question raised through this same seam has no blocked path. The
		// rule is then left out rather than printed with an empty subject, which
		// reads as a rule that fired on nothing.
		note := ""
		if ev.Path != "" {
			note = ev.Rule + " " + g.Dot + " " + ev.Path
		}
		out = append(out, trailed(render.WrapText(ev.Text, body, th.Status(StatusWarn).With(render.Bold)), note, th, body)...)

	case ui.KindApprovalDone:
		add(render.Styled(g.Arrow+" "+ev.Text, th.Muted()))

	case ui.KindLease:
		add(render.Styled(ev.Text, th.Status(StatusInfo)))

	case ui.KindUsage:
		tokens := groupDigits(ev.InputTokens) + " in / " + groupDigits(ev.OutputTokens) + " out"
		// The cost and the split by model, composed by whoever priced the
		// usage; see ui.ModelUsage. One model's cost sits beside the count, as
		// the turn's closing line; a split by model keeps a row for each.
		var priced []string
		for _, line := range strings.Split(ev.Text, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				priced = append(priced, line)
			}
		}
		if len(priced) == 1 {
			addWrapped(tokens+" "+g.Dot+" "+priced[0], th.Subtle())
			break
		}
		add(render.Styled(tokens, th.Subtle()))
		for _, line := range priced {
			addWrapped("  "+line, th.Subtle())
		}

	case ui.KindReport:
		add(render.Styled(ev.Text, th.Strong()))

	case ui.KindError:
		addCapped(ev.Text, th.Status(StatusBlock))

	default:
		addCapped(e.text(), th.Muted())
	}

	out = e.qualify(out, th, body)

	if len(out) == 0 {
		out = append(out, render.Line{})
	}
	for i := ready; i < len(out); i++ {
		out[i] = e.finishRow(th, g, body, i, out[i])
	}
	return out
}

// qualify appends the qualification block. It is appended for every kind
// rather than only for policy events, because a tool result that a grant
// allowed carries the same fields and must carry the same disclosure — and for
// a folded entry as for an open one, because a fold that could hide it would
// make an unexamined result look examined.
func (e *Entry) qualify(out []render.Line, th Theme, body int) []render.Line {
	ev := e.Event
	addWrapped := func(text string, s render.Style) {
		out = append(out, render.WrapText(text, body, s)...)
	}
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
	return out
}

// summarized reports whether the entry is drawn as its one-line summary. A
// folded tool result is not: it has a row of its own that names the call and
// what came back, and one that failed keeps the head of what it printed.
func (e *Entry) summarized() bool {
	return e.Folded && e.Event.Kind != ui.KindToolResult
}

// routine reports whether the entry is a row of the work log with nothing to
// flag: a call, a thought, or a result that passed with no qualification.
func (e *Entry) routine() bool {
	switch e.Event.Kind {
	case ui.KindReasoning:
		return true
	case ui.KindToolStart:
		// One with no result yet keeps the tool mark. It is running, or it
		// never came back, and under the routine mark it differed from the
		// passes around it by the tense of its verb alone.
		return e.result != nil
	case ui.KindToolResult:
		return e.Status() == StatusPass
	}
	return false
}

// clock is the event's time of day, or empty when it carries none.
func (e *Entry) clock() string {
	if e.Event.At.IsZero() {
		return ""
	}
	return e.Event.At.Local().Format("15:04")
}

// stamped puts a time at the right edge of a row when there is room for both,
// and leaves the row alone when there is not: the time is the part to lose.
func stamped(l render.Line, stamp string, width int, fill, quiet render.Style) render.Line {
	w := render.StringWidth(stamp)
	if stamp == "" || width < 30 || l.Width()+2+w > width {
		return l.Truncate(width)
	}
	return l.Pad(width-w, fill).Append(stamp, quiet)
}

// resultHead names the call a result answers: the call itself when the result
// came straight after it, otherwise the tool the result says it is from, or
// nothing when it says none.
func (e *Entry) resultHead(th Theme) render.Line {
	state := callDone
	if e.Event.IsError {
		state = callFailed
	}
	switch {
	case e.call != nil:
		return callLine(e.call.Event, th, state)
	case e.under:
		return nil
	case e.Event.Tool != "":
		return callLine(ui.Event{Tool: e.Event.Tool}, th, state)
	}
	return nil
}

// outcomeRow is a result on one row: the call, and what came back.
//
// One line of output is shown whole beside the call when the row has room for
// it. Anything longer is counted, never sampled into the row as if it were all
// of it: a result that names no call shows its first line with the count of the
// lines after it, and one that names its call shows the count alone.
func (e *Entry) outcomeRow(head render.Line, text string, th Theme, body int, plain render.Style) render.Line {
	g := th.Glyphs()
	text = strings.TrimSpace(text)
	lines := 0
	if text != "" {
		lines = strings.Count(text, "\n") + 1
	}
	first := text
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		first = text[:i]
	}
	// Measured only when it could fit: a first line of megabytes is not walked
	// to learn that it does not.
	fits := func(room int) bool {
		return room > 0 && len(first) <= room*4 && render.StringWidth(expandTabs(first)) <= room
	}
	counted := func(n int) string {
		if n == 1 {
			return "1 line"
		}
		return groupDigits(n) + " lines"
	}
	sep := "  " + g.Dot + " "

	if len(head) == 0 {
		switch {
		case lines == 0:
			return render.Styled("(no output)", th.Subtle())
		case lines == 1 && fits(body):
			return render.Styled(expandTabs(first), plain)
		case lines == 1:
			return render.Styled(counted(1), th.Subtle())
		}
		more := sep + "+" + counted(lines-1)
		room := body - render.StringWidth(more)
		if !fits(room) {
			return render.Styled(counted(lines), th.Subtle())
		}
		return render.Styled(expandTabs(first), plain).Append(more, th.Subtle())
	}

	if e.call != nil && confirms(e.call.Event, text) {
		// "Edited a.go" followed by the tool saying it patched a.go is the
		// same thing twice. The line is still there when the row is opened.
		return head.Truncate(body)
	}
	note, noteStyle := counted(lines), th.Subtle()
	switch {
	case lines == 0:
		note = "no output"
	case lines == 1 && fits(body-head.Width()-render.StringWidth(sep)):
		note, noteStyle = expandTabs(first), plain
	}
	// The call gives way to the count, not the other way round: a long command
	// is cut, and how much it printed is still said.
	room := body - render.StringWidth(sep) - render.StringWidth(note)
	if room < 8 {
		return head.Truncate(body)
	}
	return head.Truncate(room).Append(sep, th.Subtle()).Append(note, noteStyle)
}

// trailed puts a short note after the last of some rows, on the same row when
// there is room and on one of its own when there is not.
func trailed(rows []render.Line, note string, th Theme, body int) []render.Line {
	if note == "" {
		return rows
	}
	lead := "  " + th.Glyphs().Dot + " "
	if n := len(rows); n > 0 && rows[n-1].Width()+render.StringWidth(lead+note) <= body {
		rows[n-1] = rows[n-1].Append(lead+note, th.Subtle())
		return rows
	}
	return append(rows, render.WrapText(note, body, th.Subtle())...)
}

// How much of an entry is drawn before it is asked for.
const (
	// previewErrLines is the head of a folded tool result that failed, which is
	// the result a reader came for. One that worked folds to a single row.
	previewErrLines = 8
	// thoughtTail is the rows of a thought shown while it arrives.
	thoughtTail = 6
	// tailSpan bounds the text a live thought lays out each frame. A thought has
	// no row cache, so without a bound every delta wrapped it from its first
	// byte; see tailRows.
	tailSpan = 8 << 10
)

// preview is a folded tool result that failed: its first lines, and a row saying
// how much follows, or "" when nothing does.
//
// Only the head is laid out. The text handed to the wrapper is bounded by what
// the preview can show, so a result that is one line of several megabytes costs
// a few rows' worth of work rather than all of it.
func (e *Entry) preview(th Theme, body int, plain render.Style) (rows []render.Line, hidden string) {
	limit := previewErrLines
	head, more, clipped := previewHead(e.text(), limit, body)
	rows = renderPre(head, th, body, plain)
	if len(rows) > limit {
		rows, clipped = rows[:limit], true
	}
	switch {
	case more == 1:
		hidden = "… +1 line"
	case more > 1:
		hidden = "… +" + groupDigits(more) + " lines"
	case clipped:
		hidden = "… continues"
	}
	return rows, hidden
}

// previewHead is the text a preview of limit rows, body columns wide, lays out:
// the first limit lines, cut short when they are longer than the rows could
// show. more is how many lines follow the head, and clipped whether the head
// itself was cut.
func previewHead(text string, limit, body int) (head string, more int, clipped bool) {
	text = strings.TrimRight(text, "\n")
	head = text
	for i, n := 0, 0; i < len(text); i++ {
		j := strings.IndexByte(text[i:], '\n')
		if j < 0 {
			break
		}
		i += j
		if n++; n == limit {
			head, more = text[:i], strings.Count(text[i+1:], "\n")+1
			break
		}
	}
	// A column holds at least one byte of any text that has width, so limit rows
	// are never more than limit*body bytes; four times that is room for runes
	// that are several bytes to the column.
	if budget := limit * body * 4; len(head) > budget {
		cut := budget
		for cut > 0 && !utf8.RuneStart(head[cut]) {
			cut--
		}
		head, clipped = head[:cut], true
	}
	return head, more, clipped
}

// tailRows is the last n rows of a thought still arriving.
//
// The layout starts at a line break, never partway into a line, so the rows
// above the newest do not shift as text is added. A thought that is one long
// paragraph has no break to start at; it starts at a fixed multiple of half the
// span, which moves once per that much text instead of with every delta.
func tailRows(text string, width int, s render.Style, n int) []render.Line {
	text = strings.TrimRight(text, " \t\n")
	start := 0
	for i, seen := len(text), 0; i > 0; {
		j := strings.LastIndexByte(text[:i], '\n')
		if j < 0 {
			break
		}
		if seen++; seen == n {
			start = j + 1
			break
		}
		i = j
	}
	if len(text)-start > tailSpan {
		start = (len(text) - tailSpan/2) / (tailSpan / 2) * (tailSpan / 2)
		for start < len(text) && !utf8.RuneStart(text[start]) {
			start++
		}
	}
	rows := render.WrapText(text[start:], width, s)
	if len(rows) > n {
		rows = rows[len(rows)-n:]
	}
	return rows
}

// finishRow puts row i of an entry's body into its final form.
func (e *Entry) finishRow(th Theme, g Glyphs, body, i int, l render.Line) render.Line {
	// Continuation rows are indented to line up under the marker.
	if i > 0 {
		return render.Styled(gutterPad, th.Base()).Concat(l)
	}
	// The same attribution the folded summary carries. Expanding a child's
	// entry is how a reader looks at what the child did, so the expanded form
	// is the last place it can be dropped. The summary names the child itself.
	if e.Event.Agent != "" && !e.summarized() {
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
// wrapping can both change as it grows. The one exception is a long plain
// paragraph, which has no span to change and wraps greedily: see openLine.
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
		sl.open = openLine{}
	}
	open := text[len(sl.src):]
	if body >= openMinWidth && sl.md.settled(open) {
		if out, ready, ok := e.openRows(sl, th, body, g, open); ok {
			return out, ready
		}
	}
	sl.open = openLine{}
	// A copy, because build appends to what it returns and the entry's cache
	// keeps it; sl.rows grows in place on the next delta.
	out = make([]render.Line, len(sl.rows), len(sl.rows)+4)
	copy(out, sl.rows)
	md := sl.md
	return md.line(out, open, th, body), len(sl.rows)
}

// openRows is streamRows' last step for an open line that is a settled
// paragraph: the rows kept so far, then the wrap of the text after them, with
// every row but the last openKeep moved into the kept ones. ok is false when
// the cache cannot vouch for the rows it would return, and the caller lays the
// line out whole.
func (e *Entry) openRows(sl *streamSlot, th Theme, body int, g Glyphs, open string) (out []render.Line, ready int, ok bool) {
	oc := &sl.open
	if oc.base != len(sl.rows) || !strings.HasPrefix(open, oc.prefix) {
		*oc = openLine{base: len(sl.rows)}
	}
	tail := open[len(oc.prefix):]
	// The cache models a break as the blanks ' ' and '\t' and nothing else.
	// Wrap treats any word of unicode.IsSpace runes as a break too — a no-break
	// space, an ideographic one, U+2028, a carriage return — but trims and
	// splits only on the two, so a row can end or be empty in ways the kept
	// rows do not model. The kept prefix has none, as each tail is checked
	// before it is kept; a line that has one is laid out whole.
	if strings.IndexFunc(tail, otherSpace) >= 0 {
		return nil, 0, false
	}
	rows := parseInlineSpans(tail, th, th.Base()).Wrap(body)
	// A wrap that returns no rows has nothing to vouch for. A tail narrower
	// than the pane is not trimmed, so its trailing blanks stay; after kept
	// rows it can only be that if they end where they should not.
	if len(rows) == 0 || len(oc.prefix) > 0 && len(rows) < openKeep {
		return nil, 0, false
	}
	// Rows are kept only while the text ends on a whole rune. The tail of a
	// multi-byte rune cut between deltas is counted as invalid bytes, a column
	// each, and the rune may complete to a width of zero — a tag character, a
	// variation selector — which no row-count margin covers: the rows laid out
	// around it would be rows a whole render does not have.
	if n := len(rows) - openKeep; n > 1 && !endsInPartialRune(tail) {
		// Where each kept row ends, and the next begins after the blanks the
		// wrap dropped at the break. A row that is not where the text says it
		// is, or is empty, means the wrap did something this does not model.
		pos := 0
		for _, r := range rows[:n] {
			t := r.Text()
			if t == "" || !strings.HasPrefix(tail[pos:], t) {
				return nil, 0, false
			}
			pos += len(t)
			for pos < len(tail) && (tail[pos] == ' ' || tail[pos] == '\t') {
				pos++
			}
		}
		if next := rows[n].Text(); next == "" || !strings.HasPrefix(tail[pos:], next) {
			return nil, 0, false
		}
		for _, r := range rows[:n] {
			oc.rows = append(oc.rows, e.finishRow(th, g, body, oc.base+len(oc.rows), r))
		}
		oc.prefix = open[:len(oc.prefix)+pos]
		rows = rows[n:]
	}
	out = make([]render.Line, 0, len(sl.rows)+len(oc.rows)+len(rows)+4)
	out = append(out, sl.rows...)
	out = append(out, oc.rows...)
	ready = len(out)
	return append(out, rows...), ready, true
}

// otherSpace is a rune Wrap's strings.TrimSpace calls blank that is not one of
// the two blanks it splits and trims on.
func otherSpace(r rune) bool { return r != ' ' && r != '\t' && unicode.IsSpace(r) }

// endsInPartialRune reports whether s ends partway through a multi-byte rune,
// one whose remaining bytes may still arrive.
func endsInPartialRune(s string) bool {
	for i := 1; i <= utf8.UTFMax-1 && i <= len(s); i++ {
		if c := s[len(s)-i]; utf8.RuneStart(c) {
			return c >= utf8.RuneSelf && !utf8.FullRuneInString(s[len(s)-i:])
		}
	}
	return false
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
	st, fill := th.Status(e.Status()), th.Base()
	switch {
	case e.Event.Kind == ui.KindTurnStart:
		// The prompt mark, in the brand colour.
		st = render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}
	case e.Event.Kind == ui.KindReasoning:
		st = th.Subtle()
	}
	if th.Unicode && e.routine() {
		// One quiet mark for a row with nothing to flag: a call that has
		// returned, a result that passed, a thought. A column of ticks, gears and stars made every row
		// look like it had something to say, and the rows that do — a failure, a
		// grant, a caution — were six shapes among six. Without Unicode the
		// marks are words ("ok", ">>") and stay as they are: there a running
		// call and a finished one would otherwise differ by tense alone.
		marker, st = g.Bullet, th.Subtle()
	}
	pad := gutter - render.StringWidth(marker)
	if pad < 1 {
		pad = 1
	}
	return render.Styled(marker, st).
		Append(strings.Repeat(" ", pad), fill).
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

	// The argument a call is about — the command it runs, the path it reads —
	// leads, and stands without its key: "jira_search  paste" says what
	// "exec  command=go test ./..." says. The first of these the call carries
	// is the one; the order is fixed, so it does not vary between frames.
	var parts []string
	subject := ""
	for _, k := range subjectKeys {
		if s, ok := fields[k].(string); ok && strings.TrimSpace(s) != "" {
			subject = k
			parts = append(parts, render.TruncateWidth(oneLine(s), 96, "…"))
			break
		}
	}
	for _, k := range keys {
		if k == subject {
			continue
		}
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
	return strings.Join(parts, "  ")
}

// subjectKeys are the argument names that say what a tool call is about, in
// the order they are looked for.
var subjectKeys = []string{"command", "cmd", "path", "file_path", "file", "pattern", "query", "url"}

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

// hasInlineMarkup reports whether a line holds anything parseInlineSpans would
// style: a code span or bold. A line without any is one span in the base style,
// and is the only kind whose rows an open-line cache may keep (see settled).
func hasInlineMarkup(line string) bool {
	return strings.Contains(line, "`") || strings.Contains(line, "**")
}

func parseInlineSpans(line string, th Theme, baseStyle render.Style) render.Line {
	if !hasInlineMarkup(line) {
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
				// The informational colour, not the brand's red: red text in the
				// middle of a sentence reads as the part that went wrong.
				res = res.Append(code, th.Status(StatusInfo))
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
		return render.Styled(raw, render.Style{Fg: th.FgSubtle, Bg: th.Bg, Attrs: render.Italic})
	}

	if strings.Contains(raw, "\"") || strings.Contains(raw, "'") {
		var line render.Line
		inQuote := false
		var quoteChar rune
		var cur strings.Builder
		for _, r := range raw {
			if !inQuote && (r == '"' || r == '\'') {
				if cur.Len() > 0 {
					line = line.Append(cur.String(), render.Style{Fg: th.Fg, Bg: th.Bg})
					cur.Reset()
				}
				inQuote = true
				quoteChar = r
				cur.WriteRune(r)
			} else if inQuote && r == quoteChar {
				cur.WriteRune(r)
				line = line.Append(cur.String(), render.Style{Fg: th.FgMuted, Bg: th.Bg})
				cur.Reset()
				inQuote = false
			} else {
				cur.WriteRune(r)
			}
		}
		if cur.Len() > 0 {
			style := render.Style{Fg: th.Fg, Bg: th.Bg}
			if inQuote {
				style = render.Style{Fg: th.FgMuted, Bg: th.Bg}
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
			line = line.Append(w, render.Style{Fg: th.Info, Bg: th.Bg, Attrs: render.Bold})
		} else {
			line = line.Append(w, render.Style{Fg: th.Fg, Bg: th.Bg})
		}
	}

	for _, r := range raw {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			cur.WriteRune(r)
		} else {
			flushWord()
			line = line.Append(string(r), render.Style{Fg: th.FgMuted, Bg: th.Bg})
		}
	}
	flushWord()
	return line
}

func renderMarkdown(text string, th Theme, width int, isToolResult bool) []render.Line {
	if text == "" {
		return nil
	}
	if isToolResult {
		return renderPre(text, th, width, th.Base())
	}
	var out []render.Line
	var st mdState
	for _, raw := range strings.Split(text, "\n") {
		out = st.line(out, raw, th, width)
	}
	return out
}

// renderPre lays out text a tool printed: every line as it was written, in the
// plain style, with a diff coloured once it has shown itself to be one. Nothing
// in it is read as markdown. A "#" opening a shell comment is not a heading and
// a "*" opening a line of a listing is not a bullet, and drawing them as such
// rewrote what the tool said.
func renderPre(text string, th Theme, width int, plain render.Style) []render.Line {
	if text == "" {
		return nil
	}
	var out []render.Line
	st := mdState{pre: true, plain: plain}
	for _, raw := range strings.Split(text, "\n") {
		out = st.line(out, raw, th, width)
	}
	return out
}

// expandTabs writes a line's tabs as the blanks to the next four-column stop.
// The buffer drops a tab as it drops every control character, so a line drawn
// with its tabs lost its indentation: every Go file a tool read arrived
// flush left.
func expandTabs(s string) string {
	if !strings.Contains(s, "\t") {
		return s
	}
	var b strings.Builder
	col := 0
	for _, r := range s {
		if r == '\t' {
			n := 4 - col%4
			b.WriteString("    "[:n])
			col += n
			continue
		}
		b.WriteRune(r)
		col += render.RuneWidth(r)
	}
	return b.String()
}

// mdState is everything renderMarkdown carries from one line to the next:
// whether a fence is open, and its language. Every other construct here is
// decided by its own line alone — nothing looks ahead to the next line or back
// past the previous break — and that is the property a growing reply's
// incremental render (Entry.streamRows) is built on. A construct that did look
// across lines, a table sized to its widest row or a list renumbered by its
// successors, would have to be carried here or end that reuse.
//
// The two fields after those are set only for text a tool printed (renderPre),
// which no stream cache is kept for: pre turns markdown off, and diff records
// that a hunk header has gone by, which is carried exactly because it is the
// one thing here a later line's kind depends on.
type mdState struct {
	inCode bool
	lang   string

	pre   bool
	diff  bool
	plain render.Style
}

// mdKind is what a raw line is, decided by mdState.kind.
type mdKind uint8

const (
	mdParagraph mdKind = iota
	mdFence
	mdCode
	mdDiffHeader
	mdHunk
	mdAdded
	mdRemoved
	mdHeading1
	mdHeading2
	mdHeading3
	mdBullet
	mdOrdered
	mdQuote
	mdRule
)

// mdPrefixSpan is how much of a line kind reads: the longest literal it tests
// for is "diff --git ". Once a line's trimmed text is this long, no further
// byte can change what kind says about it, which is what lets a long open line
// be treated as a paragraph for good (see settled).
const mdPrefixSpan = len("diff --git ")

// kind classifies one raw line. It is the single place the constructs are told
// apart, and the order of the cases is the precedence: a fence line is a fence
// whether or not a fence is open, and nothing inside one is anything else.
//
// A diff is recognised in two places only: a fence that says it is one, and
// text a tool printed once a hunk header has appeared in it. It used to be
// recognised anywhere, ahead of the list test, so every "- item" a reply wrote
// was drawn as a removed line — red, on the code surface, with its inline
// markup left as backticks and asterisks.
func (st *mdState) kind(raw string) mdKind {
	if st.pre {
		switch {
		case strings.HasPrefix(raw, "diff --git "), strings.HasPrefix(raw, "@@ -"), strings.HasPrefix(raw, "@@@ -"):
			return diffKind(raw)
		case st.diff:
			return diffKind(raw)
		}
		return mdParagraph
	}
	trimmed := strings.TrimSpace(raw)
	switch {
	case strings.HasPrefix(trimmed, "```"):
		return mdFence
	case st.inCode:
		if st.lang == "diff" || st.lang == "patch" {
			if k := diffKind(raw); k != mdParagraph {
				return k
			}
		}
		return mdCode
	case strings.HasPrefix(raw, "# "):
		return mdHeading1
	case strings.HasPrefix(raw, "## "):
		return mdHeading2
	case strings.HasPrefix(raw, "### "):
		return mdHeading3
	case trimmed == "---" || trimmed == "***" || trimmed == "___":
		return mdRule
	case strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "- "):
		return mdBullet
	case strings.HasPrefix(trimmed, "> "):
		return mdQuote
	case orderedMarker(trimmed) > 0:
		return mdOrdered
	}
	return mdParagraph
}

// diffKind classifies a line of a unified diff, and answers mdParagraph for a
// context line.
func diffKind(raw string) mdKind {
	switch {
	case strings.HasPrefix(raw, "diff --git "), strings.HasPrefix(raw, "index "),
		strings.HasPrefix(raw, "--- "), strings.HasPrefix(raw, "+++ "):
		return mdDiffHeader
	case strings.HasPrefix(raw, "@@"):
		return mdHunk
	case strings.HasPrefix(raw, "+"):
		return mdAdded
	case strings.HasPrefix(raw, "-"):
		return mdRemoved
	}
	return mdParagraph
}

// orderedMarker is the length of a numbered-list marker opening s — "1. ",
// "12) " — or 0. At most three digits, which keeps the test inside the span
// settled relies on.
func orderedMarker(s string) int {
	n := 0
	for n < len(s) && n < 3 && s[n] >= '0' && s[n] <= '9' {
		n++
	}
	if n == 0 || n+1 >= len(s) || (s[n] != '.' && s[n] != ')') || s[n+1] != ' ' {
		return 0
	}
	return n + 2
}

// settled reports whether raw, a line still arriving, is a plain paragraph
// that no text added to it can turn into anything else: its rows are then the
// wrap of its text, and a longer text is the same wrap with more on the end.
//
// Three things could still change it, and each is closed. The kind: every
// test in kind is a prefix test on raw or on its trimmed form, so it is
// decided once trimmed holds mdPrefixSpan bytes — trimmed, not raw, because a
// "-" followed only by blanks is not yet a list item and a delta carrying text
// makes it one. A fence, which is a state and not a kind, is excluded by
// inCode. And the inline markup, which restyles part of a line, is excluded
// by its absence anywhere in it; a marker that arrives later takes the line
// out of this class, and it goes back to the full layout.
func (st *mdState) settled(raw string) bool {
	return !st.inCode &&
		len(strings.TrimSpace(raw)) >= mdPrefixSpan &&
		st.kind(raw) == mdParagraph &&
		!hasInlineMarkup(raw)
}

// line renders one raw line, without its break, appending its rows to out.
func (st *mdState) line(out []render.Line, raw string, th Theme, width int) []render.Line {
	trimmed := strings.TrimSpace(raw)

	switch k := st.kind(raw); k {
	case mdFence:
		// A fence draws nothing itself. The block it opens is told from the
		// prose around it by its surface and the rule down its left edge, which
		// say the same thing as a box did in two rows fewer.
		if st.inCode = !st.inCode; st.inCode {
			st.lang = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(trimmed, "```")))
		}
		return out

	case mdCode:
		return codeRows(out, highlightCodeLine(expandTabs(raw), th, st.lang), th, width)

	case mdDiffHeader, mdHunk, mdAdded, mdRemoved:
		if st.inCode {
			return codeRows(out, diffLine(k, expandTabs(raw), th, th.Bg), th, width)
		}
		st.diff = true
		return append(out, diffLine(k, expandTabs(raw), th, th.Bg).Wrap(width)...)

	case mdHeading1:
		style := render.Style{Fg: th.Accent, Bg: th.Bg, Attrs: render.Bold}
		return append(out, parseInlineSpans(strings.TrimPrefix(raw, "# "), th, style).Wrap(width)...)
	case mdHeading2:
		return append(out, parseInlineSpans(strings.TrimPrefix(raw, "## "), th, th.Strong()).Wrap(width)...)
	case mdHeading3:
		style := render.Style{Fg: th.FgMuted, Bg: th.Bg, Attrs: render.Bold}
		return append(out, parseInlineSpans(strings.TrimPrefix(raw, "### "), th, style).Wrap(width)...)

	case mdRule:
		glyph := "─"
		if !th.Unicode {
			glyph = "-"
		}
		n := width
		if n > 32 {
			n = 32
		}
		return append(out, render.Styled(strings.Repeat(glyph, n), render.Style{Fg: th.Border, Bg: th.Bg}))

	case mdBullet, mdOrdered:
		// The marker, then the item wrapped beside it rather than under it: a
		// second row that returned to the margin read as a new paragraph. A
		// nested item keeps its depth, up to a point a narrow pane can afford.
		mark, rest := th.Glyphs().Bullet, trimmed[2:]
		if k == mdOrdered {
			n := orderedMarker(trimmed)
			mark, rest = trimmed[:n-1], trimmed[n:]
		}
		depth := render.StringWidth(expandTabs(raw[:len(raw)-len(strings.TrimLeft(raw, " \t"))]))
		if depth > 8 {
			depth = 8
		}
		lead := strings.Repeat(" ", depth) + mark + " "
		leadW := render.StringWidth(lead)
		if leadW+8 > width {
			lead, leadW = mark+" ", render.StringWidth(mark)+1
		}
		rows := parseInlineSpans(strings.TrimSpace(rest), th, th.Base()).Wrap(width - leadW)
		for i, r := range rows {
			if i == 0 {
				out = append(out, render.Styled(lead, th.Muted()).Concat(r))
				continue
			}
			out = append(out, render.Styled(strings.Repeat(" ", leadW), th.Base()).Concat(r))
		}
		return out

	case mdQuote:
		bar := render.Styled(th.Glyphs().VBar+" ", render.Style{Fg: th.ruleColor(), Bg: th.Bg})
		quoted := strings.TrimSpace(strings.TrimPrefix(trimmed, ">"))
		for _, r := range parseInlineSpans(quoted, th, th.Muted()).Wrap(width - 2) {
			out = append(out, bar.Concat(r))
		}
		return out
	}

	if st.pre {
		return append(out, render.Styled(expandTabs(raw), st.plain).Wrap(width)...)
	}
	return append(out, parseInlineSpans(raw, th, th.Base()).Wrap(width)...)
}

// codeRows appends a line of a code block: a rule, then the line, wrapped when
// it is longer than the pane. It was cut at the edge, which ended a long line
// in an ellipsis and left its end unreadable at any width. The rule is what
// sets the block apart; it sits on the transcript's own ground, not a fill.
func codeRows(out []render.Line, l render.Line, th Theme, width int) []render.Line {
	rule := render.Styled(th.Glyphs().VBar+" ", render.Style{Fg: th.ruleColor(), Bg: th.Bg})
	inner := width - rule.Width()
	if inner < 1 {
		inner = 1
	}
	rows := l.Wrap(inner)
	if len(rows) == 0 {
		rows = []render.Line{nil}
	}
	for _, r := range rows {
		out = append(out, rule.Concat(r).Pad(width, render.Style{Bg: th.Bg}).Truncate(width))
	}
	return out
}

// diffLine styles a line of a unified diff on a surface, as it was written: the
// sign stays in its own column, so added, removed and context lines keep the
// alignment the diff has.
func diffLine(k mdKind, raw string, th Theme, bg render.Color) render.Line {
	style := render.Style{Fg: th.Fg, Bg: bg}
	switch k {
	case mdDiffHeader:
		style = render.Style{Fg: th.FgMuted, Bg: bg, Attrs: render.Bold}
	case mdHunk:
		style = render.Style{Fg: th.Info, Bg: bg}
	case mdAdded:
		style = render.Style{Fg: th.Success, Bg: bg}
	case mdRemoved:
		style = render.Style{Fg: th.Danger, Bg: bg}
	}
	return render.Styled(raw, style)
}
