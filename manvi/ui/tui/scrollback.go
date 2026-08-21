package tui

import (
	"strconv"
	"strings"

	"manvi/ui"
	"manvi/ui/render"
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
	// dropped counts entries retired by that bound. It is surfaced in the
	// viewport rather than kept private: a transcript that silently loses its
	// beginning is a transcript that cannot be used as evidence.
	dropped int

	// layout is the flattened row index, rebuilt when the width or the content
	// changes.
	layout    []rowRef
	layoutW   int
	layoutGen int
	gen       int

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
}

// rowRef points a rendered row back at the entry that produced it.
type rowRef struct {
	entry int
	line  render.Line
	// first marks the entry's opening row, which is where selection highlights
	// and fold markers anchor.
	first bool
}

// NewScrollback builds an empty transcript.
func NewScrollback() *Scrollback {
	return &Scrollback{follow: true, selected: -1, MaxEntries: 5000, barCol: -1}
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
	s.gen++
	if ev.Kind == ui.KindText && len(s.entries) > 0 {
		last := s.entries[len(s.entries)-1]
		if last.Event.Kind == ui.KindText {
			last.Body.WriteString(ev.Text)
			last.Foldable = strings.Contains(last.text(), "\n") || len(last.text()) > 200
			last.Invalidate()
			return
		}
	}

	e := &Entry{Event: ev}
	if ev.Kind == ui.KindText {
		e.Body.WriteString(ev.Text)
	}
	e.Foldable = foldable(ev)
	s.entries = append(s.entries, e)

	if s.MaxEntries > 0 && len(s.entries) > s.MaxEntries {
		drop := len(s.entries) - s.MaxEntries
		s.entries = append([]*Entry(nil), s.entries[drop:]...)
		s.dropped += drop
		if s.selected >= 0 {
			s.selected -= drop
			if s.selected < 0 {
				s.selected = -1
			}
		}
	}
}

func foldable(ev ui.Event) bool {
	switch ev.Kind {
	case ui.KindToolResult, ui.KindReasoning, ui.KindText:
		return strings.Contains(ev.Text, "\n") || len(ev.Text) > 200
	}
	return false
}

// rows rebuilds the flat row index if the width or content changed.
func (s *Scrollback) rows(th Theme, width int) []rowRef {
	if s.layout != nil && s.layoutW == width && s.layoutGen == s.gen {
		return s.layout
	}
	out := make([]rowRef, 0, len(s.entries)*2)
	for i, e := range s.entries {
		lines := e.Lines(th, width)
		for j, l := range lines {
			out = append(out, rowRef{entry: i, line: l, first: j == 0})
		}
	}
	s.layout, s.layoutW, s.layoutGen = out, width, s.gen
	return out
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

// touch marks the layout stale, for a change that alters rendering without
// adding an entry — folding, or a theme switch.
func (s *Scrollback) touch() { s.gen++ }

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
		if r.entry != s.selected {
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
	s.touch()
	return true
}

// SetFoldAll folds or unfolds everything foldable.
func (s *Scrollback) SetFoldAll(folded bool) {
	for _, e := range s.entries {
		if e.Foldable {
			e.Folded = folded
		}
	}
	s.touch()
}

// SelectedText is the selected entry's body, for copying.
func (s *Scrollback) SelectedText() string {
	e := s.Selected()
	if e == nil {
		return ""
	}
	return e.text()
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
		line := rows[i].line
		y := body.Y + row
		line.Truncate(body.W).Draw(b, body.X, y)
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
	track := render.Style{Fg: th.Border, Bg: th.Bg}
	thumb := render.Style{Fg: th.AccentDim, Bg: th.Bg}
	glyph := "│"
	block := "┃"
	if !th.Unicode {
		glyph, block = "|", "#"
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
