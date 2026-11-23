package tui

import (
	"strconv"
	"strings"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

func itoa(n int) string { return strconv.Itoa(n) }

// Scrollback is the transcript: a list of entries, a viewport over their
// rendered rows, and a selection.
//
// Two coordinate systems meet here and confusing them is the classic scrollback
// bug. Rows are what the viewport scrolls through, and there are many per
// entry once wrapping is applied. Entries are what selection and folding move
// between. Every public method says which one it works in.
type Scrollback struct {
	entries []*Entry

	// scroll is the first visible row, counted from the top of the rendered
	// transcript.
	scroll int
	// follow keeps the viewport pinned to the newest row as events arrive. Any
	// manual scroll clears it; scrolling back to the bottom restores it. Without
	// this an operator who scrolls up to read something is dragged away from it
	// by the next event.
	follow bool
	// selected indexes entries, or -1 for no selection.
	selected int

	// MaxEntries bounds retention. Zero means unbounded.
	MaxEntries int
	// MaxBytes bounds retention by the text the entries hold. A count of
	// entries does not bound memory: one tool result can be megabytes, and the
	// entry bound alone let five thousand of them stand. Zero means unbounded.
	// The newest entry is never retired by it, so a single reply larger than
	// the bound is kept whole rather than cut out from under the reader.
	MaxBytes int
	// bytes is what the retained entries hold, kept current as entries arrive,
	// grow and retire so the bound does not rescan the transcript per event.
	bytes int
	// dropped counts entries retired by that bound. It is surfaced in the
	// viewport rather than kept private: a transcript that silently loses its
	// beginning is a transcript that cannot be used as evidence.
	dropped int

	// layout is the row index the last layout pass produced — the one the
	// last frame drew, unless a key handler has laid out since.
	layout []rowRef
	// slots keep the flattened row index at the two widths most recently laid
	// out at. Two, because a frame lays out at two widths whenever the
	// transcript is shorter than the pane: once without the scrollbar column
	// to learn that none is needed, and again at the full width it then draws
	// at. A single cache keyed on width was evicted twice every frame by that,
	// idle ticks included, and every entry was re-rendered with it.
	slots [2]layoutSlot
	// slotUse orders the slots by recency; the older one is the one a third
	// width replaces.
	slotUse [2]uint64
	useTick uint64

	// view is the rect the last frame drew into, and viewW the width that frame
	// laid the content out at.
	//
	// Kept here so callers stop passing their own guesses. A click handler and
	// a key handler each recomputing the transcript's geometry is two chances
	// to disagree with the frame the operator is actually looking at, and the
	// symptom — a click selecting the wrong row, a page-down overshooting — is
	// one nobody reports as a geometry bug.
	view  render.Rect
	viewW int
	// pad is the blank rows the last frame put above the content to bottom-align
	// it. Stored rather than recomputed in the hit test, because two copies of
	// the same offset is two chances for a click to land a row out.
	pad int
	// barCol is the screen column the last frame's scrollbar occupied, or -1
	// when nothing was scrollable enough to draw one. The click handler grabs
	// the thumb on that column rather than treating it as transcript text.
	barCol int

	// Search state.
	searchQuery string
	matches     []int
	matchIdx    int
}

// rowRef points a rendered row back at the entry that produced it.
type rowRef struct {
	entry int
	line  render.Line
	// first marks the entry's opening row, which is where selection highlights
	// and fold markers anchor.
	first bool
	// gap marks the blank row that separates this entry from the one above. It
	// belongs to the layout and not to the entry's own rows, so an entry renders
	// the same wherever it sits and the row caches never hold a spacer.
	gap bool
}

// layoutSlot is the flattened row index at one width and theme.
//
// It is kept current a tail at a time. A streamed delta changes only the last
// entry, and rebuilding every row of a long transcript for it — once per frame
// for the length of a turn — was the bulk of what a frame cost.
type layoutSlot struct {
	rows  []rowRef
	width int
	theme string
	// valid is how many leading entries rows still describes correctly. Every
	// change to an entry's rendering lowers it to that entry's index; a layout
	// pass re-renders from there to the end and leaves the prefix alone.
	valid int
	used  bool
}

// NewScrollback builds an empty transcript.
func NewScrollback() *Scrollback {
	return &Scrollback{follow: true, selected: -1, MaxEntries: 5000, MaxBytes: DefaultMaxScrollbackBytes, barCol: -1}
}

// DefaultMaxScrollbackBytes is the text a transcript retains by default.
const DefaultMaxScrollbackBytes = 32 << 20

// entryBytes is what an entry retains, each byte of text once. A streamed
// entry keeps its text in Body, and its event's Text is the first piece of
// that same text, so only the body is counted for it. No fixed per-entry
// overhead is counted: the figure is the text held and nothing else.
func entryBytes(e *Entry) int {
	n := eventBytes(e.Event)
	if e.Body.Len() > 0 {
		n -= len(e.Event.Text)
	}
	return n + e.Body.Len()
}

// eventBytes is what an event holds that can be large: its text, tool
// arguments, diagnostic payload and the lists of strings it carries.
func eventBytes(ev ui.Event) int {
	n := len(ev.Text) + len(ev.Detail) + len(ev.Tool) + len(ev.Path) + len(ev.Arguments) + len(ev.Data)
	for _, list := range [][]string{ev.Choices, ev.Paths, ev.Findings, ev.SelectedTests} {
		for _, item := range list {
			n += len(item)
		}
	}
	return n
}

// ScrollbarCol is the screen column of the last frame's scrollbar, or -1 when
// no scrollbar was drawn.
func (s *Scrollback) ScrollbarCol() int { return s.barCol }

// Len is the entry count.
func (s *Scrollback) Len() int { return len(s.entries) }

// Dropped reports how many entries the retention bound retired.
func (s *Scrollback) Dropped() int { return s.dropped }

// Following reports whether the viewport is pinned to the newest row.
func (s *Scrollback) Following() bool { return s.follow }

// Entries exposes the transcript, for the report and copy paths.
func (s *Scrollback) Entries() []*Entry { return s.entries }

// Selected returns the selected entry, or nil.
func (s *Scrollback) Selected() *Entry {
	if s.selected < 0 || s.selected >= len(s.entries) {
		return nil
	}
	return s.entries[s.selected]
}

// Append adds an event.
//
// Streamed assistant text is merged into the entry it continues rather than
// becoming one entry per delta — a turn produces hundreds of those, and each as
// its own entry would make the transcript unnavigable and the fold state
// meaningless.
func (s *Scrollback) Append(ev ui.Event) {
	// Not drawn. These kinds exist for the NDJSON face, and without this each
	// would become an entry with nothing to render; see ui.Kind.WireOnly.
	if ev.Kind.WireOnly() {
		return
	}
	if ev.Kind == ui.KindTurnEnd {
		// A turn's end has nothing to draw: it was an entry with an empty body,
		// which is a bullet on a row of its own after every turn. It still ends
		// whatever was arriving, so a thought the turn stopped on is collapsed.
		s.settleLast()
		return
	}
	// A model's thinking streams as a reply does, and for the same reason is
	// one entry. Kept apart, each chunk was a row of its own and a sentence
	// broke wherever the provider happened to cut it. A sub-agent's stream is
	// never merged into this session's: the entry says whose it is.
	if (ev.Kind == ui.KindText || ev.Kind == ui.KindReasoning) && len(s.entries) > 0 {
		last := s.entries[len(s.entries)-1]
		if last.Event.Kind == ev.Kind && last.Event.Agent == ev.Agent {
			last.Body.WriteString(ev.Text)
			s.bytes += len(ev.Text)
			if !ev.At.IsZero() {
				last.until = ev.At
			}
			// The same rule foldable applies to a whole body, decided from the
			// delta alone. A body that held a line break or ran past the bound
			// still does once more text is added to it, so the answer only ever
			// flips one way, and testing the new text is all a flip can need.
			// Scanning the accumulated body instead made each delta cost the
			// length of the reply so far.
			if !last.Foldable {
				last.Foldable = strings.Contains(ev.Text, "\n") || last.Body.Len() > 200
			}
			last.Invalidate()
			s.changed(len(s.entries) - 1)
			s.enforceBounds()
			return
		}
	}

	// Only the last entry grows, so whatever was last has stopped.
	s.settleLast()
	e := &Entry{Event: ev, until: ev.At}
	if ev.Kind == ui.KindText || ev.Kind == ui.KindReasoning {
		e.Body.WriteString(ev.Text)
	}
	e.Foldable = foldable(ev)
	switch ev.Kind {
	case ui.KindReasoning:
		// Shown as it arrives, and collapsed by settle once it has stopped.
		e.live = true
	case ui.KindToolResult:
		// A result opens folded. A read of a large file or a chatty command is
		// thousands of rows, and unfolded by default it pushed the reply it led
		// to off the screen.
		e.Folded = e.Foldable
		// One that comes straight after its call takes the call's row: a call
		// and its outcome are one thing that happened, and were two rows. Only
		// straight after — with a policy decision between them the call keeps
		// its own row, and what is attached to it stays under it.
		//
		// A result carries no id of the call it answers, so the pairing is by
		// position. The agent loop runs a response's calls one at a time, each
		// logged, run and answered before the next is logged (agent
		// dispatchCalls), so the call a result follows is the call it answers.
		// Where that is not plain from the transcript itself — two calls by the
		// same agent open at once — nothing is paired: each call and each
		// result keeps a row, and no result is drawn as another call's.
		n := len(s.entries)
		at := n - 1
		for at >= 0 && decision(s.entries[at].Event.Kind) && s.entries[at].Event.Agent == ev.Agent {
			at--
		}
		if at >= 0 && s.answers(at, ev) {
			call := s.entries[at]
			call.result = e
			if at == n-1 {
				e.call, call.absorbed = call, true
				call.Invalidate()
				s.changed(at)
				if s.selected == at {
					// The row the operator had selected is now this entry's.
					s.selected = n
				}
			} else {
				// The call keeps its row, which now says how it came out.
				e.under = true
				e.absorbed = s.restates(at, n, ev)
				call.Invalidate()
				s.changed(at)
			}
		}
	}
	s.entries = append(s.entries, e)
	s.bytes += entryBytes(e)
	s.enforceBounds()
}

// enforceBounds retires the oldest entries until the transcript is within
// both bounds: the entry count, and the bytes it holds. The newest entry
// stays whatever its size, because retiring it would drop what is on screen.
func (s *Scrollback) enforceBounds() {
	drop := 0
	if s.MaxEntries > 0 && len(s.entries) > s.MaxEntries {
		drop = len(s.entries) - s.MaxEntries
	}
	held := s.bytes
	for _, e := range s.entries[:drop] {
		held -= entryBytes(e)
	}
	for s.MaxBytes > 0 && held > s.MaxBytes && drop < len(s.entries)-1 {
		e := s.entries[drop]
		held -= entryBytes(e)
		drop++
	}
	if drop == 0 {
		return
	}
	s.entries = append([]*Entry(nil), s.entries[drop:]...)
	s.bytes = held
	s.dropped += drop
	// Every row's entry index just shifted. Rewriting them in place would
	// work, but this happens once per new entry at the bound and rows are
	// cheap to re-gather from the entries' own caches; a second, subtler
	// path is not worth it.
	s.touch()
	if s.selected >= 0 {
		s.selected -= drop
		if s.selected < 0 {
			s.selected = -1
		}
	}
}

// settleLast ends the newest entry's stream, and lays it out again when that
// changed how it is drawn.
func (s *Scrollback) settleLast() {
	if n := len(s.entries); n > 0 && s.entries[n-1].settle() {
		s.changed(n - 1)
	}
}

// answers reports whether a result is the answer to the call at an index: the
// call is this agent's, is not answered yet, is the tool the result names when
// it names one, and is the only call of this agent's still open just there.
func (s *Scrollback) answers(at int, result ui.Event) bool {
	call := s.entries[at]
	if call.Event.Kind != ui.KindToolStart || call.result != nil || call.Event.Agent != result.Agent {
		return false
	}
	if result.Tool != "" && result.Tool != call.Event.Tool {
		return false
	}
	for i := at - 1; i >= 0; i-- {
		prev := s.entries[i]
		if prev.Event.Agent != result.Agent {
			break
		}
		switch {
		case prev.Event.Kind == ui.KindToolStart && prev.result == nil:
			return false
		case prev.Event.Kind == ui.KindToolStart, decision(prev.Event.Kind):
			continue
		}
		break
	}
	return true
}

// decision reports whether a kind is a ruling on a tool call: what sits between
// a call and its result when the call was questioned.
// restates reports whether a result under its call, with the decisions at
// (call, end) between them, would only say again what is already on screen.
//
// The loop writes a gate's verdict twice: as the decision, and as the text of
// the result it then returns to the model (agent recordCall logs both from
// one Result.Text). Drawn as logged, a refusal was the same sentence on two
// rows. The same holds for a file tool's "patched a.go (9 bytes)" under a call
// already drawn as "Edited a.go".
//
// A result that carries a qualification of its own is never dropped: the grant
// or the unrun check it names is drawn under it and nowhere else.
func (s *Scrollback) restates(call, end int, result ui.Event) bool {
	text := strings.TrimSpace(result.Text)
	if text == "" || result.Qualified() {
		return false
	}
	if !result.IsError && confirms(s.entries[call].Event, text) {
		return true
	}
	for _, d := range s.entries[call+1 : end] {
		if d.Event.Kind == ui.KindPolicy && strings.TrimSpace(d.Event.Text) == text {
			return true
		}
	}
	return false
}

func decision(k ui.Kind) bool {
	switch k {
	case ui.KindPolicy, ui.KindApproval, ui.KindApprovalDone:
		return true
	}
	return false
}

func foldable(ev ui.Event) bool {
	switch ev.Kind {
	case ui.KindReasoning:
		// Always: a finished thought collapses to its heading whatever its
		// length, so the transcript reads as what was done and not as what was
		// considered on the way.
		return true
	case ui.KindToolResult:
		// Whatever it printed: a result is drawn as one row, so even a single
		// line of output can be more than the row shows.
		return strings.TrimSpace(ev.Text) != ""
	case ui.KindText, ui.KindTurnStart:
		return strings.Contains(ev.Text, "\n") || len(ev.Text) > 200
	}
	return false
}

// gapBefore reports whether a blank row separates an entry from the one above.
//
// The transcript is read in three kinds of block: what was said, by the
// operator or the model; the work between — thoughts, calls, their results and
// the decisions on them; and notices. A blank row goes between blocks and never
// inside one, so the work of a turn is one run of rows, a row to a step, with
// what was asked above it and the reply below.
func gapBefore(prev, cur *Entry) bool {
	work := func(k ui.Kind) bool {
		switch k {
		case ui.KindReasoning, ui.KindToolStart, ui.KindToolResult,
			ui.KindPolicy, ui.KindApproval, ui.KindApprovalDone:
			return true
		}
		return false
	}
	said := func(k ui.Kind) bool {
		switch k {
		case ui.KindSessionStart, ui.KindTurnStart, ui.KindHarnessMessage, ui.KindText:
			return true
		}
		return false
	}
	p, c := prev.Event.Kind, cur.Event.Kind
	switch {
	case work(c):
		return !work(p)
	case said(c):
		return true
	}
	// Notices, lease changes, usage and reports: a run of them stacks.
	return work(p) || said(p)
}

// rows brings the flat row index at a width up to date and returns it.
//
// Only entries at or after the first one that changed are laid out again; the
// rows before them are kept as they are. The theme is part of the key: an
// entry's own cache already knows a theme switch from a width change, and a
// layout that did not would go on drawing the old theme's rows.
func (s *Scrollback) rows(th Theme, width int) []rowRef {
	slot := s.slotFor(th.Name, width)
	if slot.valid < len(s.entries) {
		// Rows are grouped by entry in order, so the stale ones are a suffix.
		keep := len(slot.rows)
		for keep > 0 && slot.rows[keep-1].entry >= slot.valid {
			keep--
		}
		out := slot.rows[:keep]
		// The entry above, for the spacing: the nearest one that is drawn. A
		// call absorbed by its result has no rows, and is not what an entry
		// sits under.
		above := slot.valid - 1
		for above >= 0 && s.entries[above].absorbed {
			above--
		}
		for i := slot.valid; i < len(s.entries); i++ {
			if s.entries[i].absorbed {
				continue
			}
			// No spacer under a row that is already blank: a reply that ends in
			// a line break leaves one, and two blank rows read as a section
			// break the transcript does not have.
			if above >= 0 && gapBefore(s.entries[above], s.entries[i]) && !blankTail(out) {
				out = append(out, rowRef{entry: i, gap: true})
			}
			for j, l := range s.entries[i].Lines(th, width) {
				out = append(out, rowRef{entry: i, line: l, first: j == 0})
			}
			above = i
		}
		slot.rows, slot.valid = out, len(s.entries)
	}
	s.layout = slot.rows
	return slot.rows
}

// blankTail reports whether the last row laid out so far shows nothing.
func blankTail(rows []rowRef) bool {
	if len(rows) == 0 {
		return true
	}
	last := rows[len(rows)-1]
	if last.gap {
		return true
	}
	for _, span := range last.line {
		if strings.TrimSpace(span.Text) != "" {
			return false
		}
	}
	return true
}

// slotFor finds the layout slot for a width and theme, claiming the less
// recently used one when neither matches.
func (s *Scrollback) slotFor(theme string, width int) *layoutSlot {
	s.useTick++
	pick := -1
	for i := range s.slots {
		if s.slots[i].used && s.slots[i].width == width && s.slots[i].theme == theme {
			pick = i
			break
		}
	}
	if pick < 0 {
		pick = 0
		if s.slotUse[1] < s.slotUse[0] {
			pick = 1
		}
		// The row slice is reused rather than reallocated; a slot changes
		// width on a resize, and the rows it held are all replaced.
		s.slots[pick] = layoutSlot{rows: s.slots[pick].rows[:0], width: width, theme: theme, used: true}
	}
	s.slotUse[pick] = s.useTick
	return &s.slots[pick]
}

// retain releases any slot this frame cannot use. A slot left at a width the
// pane no longer has would otherwise hold a second copy of every rendered row
// alive until some later width happened to claim it.
func (s *Scrollback) retain(theme string, w1, w2 int) {
	for i := range s.slots {
		sl := &s.slots[i]
		if sl.used && (sl.theme != theme || (sl.width != w1 && sl.width != w2)) {
			s.slots[i] = layoutSlot{}
		}
	}
}

// changed marks entry i, and every entry after it, as needing to be laid out
// again in every slot.
func (s *Scrollback) changed(i int) {
	for k := range s.slots {
		if s.slots[k].valid > i {
			s.slots[k].valid = i
		}
	}
}

// layoutTotal is the row count from the last draw.
//
// Scrolling needs a total, and the total depends on the width the transcript
// was last laid out at. Recomputing it here from a width the caller guessed
// would let a key press and the next frame disagree about how far the content
// extends, which shows up as a viewport that will not reach its own bottom.
func (s *Scrollback) layoutTotal() int {
	n := len(s.layout)
	if s.dropped > 0 {
		n++
	}
	return n
}

// touch marks the whole layout stale, for a change that can alter any entry's
// rendering — folding everything, or retiring entries off the front.
func (s *Scrollback) touch() { s.changed(0) }

// Viewport is the row count of the last drawn frame, or a usable minimum
// before anything has been drawn.
func (s *Scrollback) Viewport() int {
	if s.view.H > 0 {
		return s.view.H
	}
	return 1
}

// ScrollBy moves the viewport by n rows, clearing follow when it moves up.
func (s *Scrollback) ScrollBy(n int) {
	viewport, total := s.Viewport(), s.layoutTotal()
	s.scroll += n
	s.clamp(viewport, total)
	s.follow = s.scroll >= maxScroll(viewport, total)
}

// ScrollToBottom pins the viewport to the newest row.
func (s *Scrollback) ScrollToBottom() {
	s.scroll = maxScroll(s.Viewport(), s.layoutTotal())
	s.follow = true
}

// ScrollToTop moves to the oldest row.
func (s *Scrollback) ScrollToTop() {
	s.scroll = 0
	s.follow = false
}

// ScrollToFraction moves the viewport to a fraction of its travel — what a
// dragged scrollbar thumb asks for. The geometry is the last drawn frame's,
// as with a click: two copies of the layout arithmetic are two chances for
// the thumb and the viewport to disagree.
func (s *Scrollback) ScrollToFraction(frac float64) {
	viewport, total := s.Viewport(), s.layoutTotal()
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	ms := maxScroll(viewport, total)
	s.scroll = int(float64(ms)*frac + 0.5)
	s.follow = s.scroll >= ms
	s.clamp(viewport, total)
}

func (s *Scrollback) clamp(viewport, total int) {
	if s.scroll > maxScroll(viewport, total) {
		s.scroll = maxScroll(viewport, total)
	}
	if s.scroll < 0 {
		s.scroll = 0
	}
}

func maxScroll(viewport, total int) int {
	if total <= viewport {
		return 0
	}
	return total - viewport
}

// SelectDelta moves the selection by n entries and scrolls it into view.
func (s *Scrollback) SelectDelta(n int, th Theme) {
	if len(s.entries) == 0 {
		return
	}
	if s.selected < 0 {
		// Entering selection from the bottom selects the newest entry, which is
		// what the operator was already looking at.
		s.selected = len(s.entries) - 1
	} else {
		s.selected += n
	}
	if s.selected < 0 {
		s.selected = 0
	}
	if s.selected >= len(s.entries) {
		s.selected = len(s.entries) - 1
	}
	// A call absorbed by its result has no row to select. The selection goes on
	// in the direction it was moving, and turns back only at the end of the
	// transcript.
	step := 1
	if n <= 0 {
		step = -1
	}
	for _, dir := range []int{step, -step} {
		i := s.selected
		for i >= 0 && i < len(s.entries) && s.entries[i].absorbed {
			i += dir
		}
		if i >= 0 && i < len(s.entries) {
			s.selected = i
			break
		}
	}
	s.revealSelected(th)
}

// SelectNone clears the selection and returns to following.
func (s *Scrollback) SelectNone() { s.selected = -1 }

// SelectAt selects whichever entry owns a screen row, for a mouse click.
//
// It reads the layout the last frame was built from rather than rebuilding one,
// so the row the operator clicked is the row that was on screen.
func (s *Scrollback) SelectAt(y int) bool {
	if s.view.H == 0 || y < s.view.Y || y >= s.view.Bottom() {
		return false
	}
	idx := s.scroll + (y - s.view.Y - s.pad)
	if s.dropped > 0 {
		idx--
	}
	if idx < 0 || idx >= len(s.layout) {
		return false
	}
	s.selected = s.layout[idx].entry
	return true
}

// Contains reports whether a screen point falls inside the last drawn viewport.
func (s *Scrollback) Contains(x, y int) bool { return s.view.Contains(x, y) }

// revealSelected scrolls until the selected entry's first row is visible.
func (s *Scrollback) revealSelected(th Theme) {
	viewport := s.Viewport()
	width := s.viewW
	if width <= 0 {
		width = 80
	}
	rows := s.rows(th, width)
	first, last := -1, -1
	for i, r := range rows {
		// The spacer above an entry is not part of it: revealing an entry means
		// showing its first row, not the blank one before it.
		if r.entry != s.selected || r.gap {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 {
		return
	}
	if first < s.scroll {
		s.scroll = first
	}
	// Scrolling a tall entry to show its end would hide its beginning, which is
	// where the marker and the summary are. Its head wins.
	if last >= s.scroll+viewport {
		s.scroll = last - viewport + 1
		if s.scroll > first {
			s.scroll = first
		}
	}
	s.clamp(viewport, len(rows))
	s.follow = s.scroll >= maxScroll(viewport, len(rows))
}

// ToggleFold folds or unfolds the selected entry.
func (s *Scrollback) ToggleFold() bool {
	e := s.Selected()
	if e == nil || !e.Foldable {
		return false
	}
	e.Folded = !e.Folded
	e.foldSet = true
	s.changed(s.selected)
	return true
}

// SetFoldAll folds or unfolds everything foldable.
func (s *Scrollback) SetFoldAll(folded bool) {
	for _, e := range s.entries {
		if e.Foldable {
			e.Folded = folded
			e.foldSet = true
		}
	}
	s.touch()
}

// SelectedText is the selected entry's body, for copying.
// If the entry contains exactly one fenced code block, it extracts only the inner code.
func (s *Scrollback) SelectedText() string {
	e := s.Selected()
	if e == nil {
		return ""
	}
	raw := e.text()
	first := strings.Index(raw, "```")
	if first >= 0 {
		afterFirst := raw[first+3:]
		if nl := strings.IndexByte(afterFirst, '\n'); nl >= 0 {
			rest := afterFirst[nl+1:]
			if end := strings.Index(rest, "```"); end >= 0 {
				if !strings.Contains(rest[end+3:], "```") {
					return strings.TrimRight(rest[:end], "\r\n")
				}
			}
		}
	}
	return raw
}

// SearchQuery returns the active search query.
func (s *Scrollback) SearchQuery() string { return s.searchQuery }

// MatchCount returns the number of matches for the active search.
func (s *Scrollback) MatchCount() int { return len(s.matches) }

// SearchStatus returns a label describing match position.
func (s *Scrollback) SearchStatus() string {
	if s.searchQuery == "" {
		return ""
	}
	if len(s.matches) == 0 {
		return "no matches: " + s.searchQuery
	}
	return "match " + itoa(s.matchIdx+1) + " of " + itoa(len(s.matches)) + ": " + s.searchQuery
}

// SetSearch filters entries matching the query and jumps to the nearest match.
func (s *Scrollback) SetSearch(query string, th Theme) bool {
	s.searchQuery = strings.TrimSpace(query)
	s.matches = nil
	s.matchIdx = 0
	if s.searchQuery == "" {
		return false
	}
	qLower := strings.ToLower(s.searchQuery)
	// Each entry's lowered text is kept between searches. Typing a query runs
	// this once per keystroke, and lowering every entry's whole text each time
	// copied the transcript per key — a large tool result alone is megabytes.
	for i, e := range s.entries {
		// A call drawn on its result's row has none of its own to jump to.
		if !e.absorbed && strings.Contains(e.lowerText(), qLower) {
			s.matches = append(s.matches, i)
		}
	}
	if len(s.matches) == 0 {
		return false
	}
	// Select the first match at or after current selection, or match 0
	s.matchIdx = 0
	if s.selected >= 0 {
		for mIdx, eIdx := range s.matches {
			if eIdx >= s.selected {
				s.matchIdx = mIdx
				break
			}
		}
	}
	s.selected = s.matches[s.matchIdx]
	if s.selected < len(s.entries) && s.entries[s.selected].Foldable && s.entries[s.selected].Folded {
		s.entries[s.selected].Folded = false
		s.changed(s.selected)
	}
	s.revealSelected(th)
	return true
}

// NextMatch jumps to the next search match.
func (s *Scrollback) NextMatch(th Theme) bool {
	if len(s.matches) == 0 {
		return false
	}
	s.matchIdx = (s.matchIdx + 1) % len(s.matches)
	s.selected = s.matches[s.matchIdx]
	if s.selected < len(s.entries) && s.entries[s.selected].Foldable && s.entries[s.selected].Folded {
		s.entries[s.selected].Folded = false
		s.changed(s.selected)
	}
	s.revealSelected(th)
	return true
}

// PrevMatch jumps to the previous search match.
func (s *Scrollback) PrevMatch(th Theme) bool {
	if len(s.matches) == 0 {
		return false
	}
	s.matchIdx = (s.matchIdx - 1 + len(s.matches)) % len(s.matches)
	s.selected = s.matches[s.matchIdx]
	if s.selected < len(s.entries) && s.entries[s.selected].Foldable && s.entries[s.selected].Folded {
		s.entries[s.selected].Folded = false
		s.changed(s.selected)
	}
	s.revealSelected(th)
	return true
}

// ClearSearch clears the active search query.
func (s *Scrollback) ClearSearch() {
	s.searchQuery = ""
	s.matches = nil
	s.matchIdx = 0
}

func (s *Scrollback) isMatch(entryIdx int) bool {
	for _, m := range s.matches {
		if m == entryIdx {
			return true
		}
	}
	return false
}

// Draw paints the viewport.
//
// Returns the total row count, which the caller needs for the scrollbar and for
// the next scroll clamp.
func (s *Scrollback) Draw(b *render.Buffer, r render.Rect, th Theme, focused bool) int {
	if r.Empty() {
		s.barCol = -1
		return 0
	}
	body := r
	s.retain(th.Name, r.W-1, r.W)
	// A scrollbar column is reserved only when there is something to scroll,
	// so a short transcript uses the full width.
	rows := s.rows(th, r.W-1)
	total := len(rows)
	if s.dropped > 0 {
		total++
	}
	scrollable := total > r.H
	if !scrollable {
		rows = s.rows(th, r.W)
		s.barCol = -1
	} else {
		body = render.Rect{X: r.X, Y: r.Y, W: r.W - 1, H: r.H}
		s.barCol = r.Right() - 1
	}

	s.view, s.viewW = body, body.W
	if s.follow {
		s.scroll = maxScroll(r.H, total)
	}
	s.clamp(r.H, total)

	b.Fill(r, ' ', th.Base())

	// Content shorter than the pane sits against the bottom, next to the
	// composer, rather than stranded at the top under a band of empty rows. A
	// transcript grows downward toward the thing you type into; top-aligning a
	// short one puts the newest line furthest from where the eye already is.
	s.pad = 0
	if total < r.H {
		s.pad = r.H - total
	}
	row := s.pad
	if s.dropped > 0 && s.scroll == 0 {
		// Never silent. A transcript missing its beginning must say so where
		// the beginning was.
		render.Styled(
			"… "+itoa(s.dropped)+" earlier entries retired by the retention bound",
			th.Status(StatusDegraded),
		).DrawIn(b, render.Rect{X: body.X, Y: body.Y + row, W: body.W, H: 1})
		row++
	}

	start := s.scroll
	if s.dropped > 0 {
		start = s.scroll - 1
		if start < 0 {
			start = 0
		}
	}
	for i := start; i < len(rows) && row < r.H; i++ {
		if rows[i].gap {
			// Blank, and left out of the selection and search tints: a
			// highlight that ran up into the spacer would read as one more row
			// of the entry.
			row++
			continue
		}
		line := rows[i].line
		y := body.Y + row
		line.Truncate(body.W).Draw(b, body.X, y)
		if s.searchQuery != "" && s.isMatch(rows[i].entry) && rows[i].entry != s.selected {
			// A lighter wash of the selection's own colour for the other
			// matches, so the selected one is the stronger of the same thing.
			band := render.Rect{X: r.X, Y: y, W: r.W, H: 1}
			b.BlendBackground(band, th.Selection, 0.3)
		}
		if rows[i].entry == s.selected {
			// A tint rather than a fill, so the severity colouring underneath —
			// which is the thing the operator is selecting in order to read —
			// survives the highlight.
			band := render.Rect{X: r.X, Y: y, W: r.W, H: 1}
			b.BlendBackground(band, th.Selection, 0.65)
			if focused && rows[i].first {
				b.SetString(r.X, y, th.Glyphs().Caret, render.Style{Fg: th.Accent, Bg: th.Selection})
			}
		}
		row++
	}

	if scrollable {
		drawScrollbar(b, render.Rect{X: r.Right() - 1, Y: r.Y, W: 1, H: r.H}, s.scroll, r.H, total, th)
	}
	return total
}

// drawScrollbar paints a proportional thumb.
func drawScrollbar(b *render.Buffer, r render.Rect, offset, viewport, total int, th Theme) {
	if r.Empty() || total <= 0 {
		return
	}
	// The track is empty and only the thumb is drawn. A rule down every row of
	// the pane was a second border beside the transcript, and the brightest
	// thing in the column least worth looking at. Position reads from the thumb
	// alone, and the column still takes a click anywhere along it.
	track := render.Style{Fg: th.Border, Bg: th.Bg}
	thumb := render.Style{Fg: th.FgSubtle, Bg: th.Bg}
	glyph := " "
	block := "┃"
	if !th.Unicode {
		block = "#"
	}

	size := r.H * viewport / total
	if size < 1 {
		size = 1
	}
	maxOff := maxScroll(viewport, total)
	pos := 0
	if maxOff > 0 {
		pos = (r.H - size) * offset / maxOff
	}
	for i := 0; i < r.H; i++ {
		if i >= pos && i < pos+size {
			b.SetString(r.X, r.Y+i, block, thumb)
		} else {
			b.SetString(r.X, r.Y+i, glyph, track)
		}
	}
}
