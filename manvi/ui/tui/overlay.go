package tui

import (
	"fmt"
	"strings"
	"time"

	"manvi/ui/render"
)

// OverlayKind is which floating surface is open.
type OverlayKind int

// The overlays.
const (
	OverlayNone OverlayKind = iota
	// OverlayComplete is the inline dropdown anchored to the prompt, for slash
	// commands and file references.
	OverlayComplete
	// OverlayPalette is the searchable command list.
	OverlayPalette
	// OverlayHelp lists every binding, grouped by context.
	OverlayHelp
	// OverlaySessions is the session picker.
	OverlaySessions
	// OverlayTheme is the theme picker.
	OverlayTheme
	// OverlaySettings is the harness settings picker: every flag, its value,
	// where the value came from, and whether it can be moved from here.
	OverlaySettings
	// OverlaySearch is the transcript search bar.
	OverlaySearch
	// OverlayRename is the session rename modal.
	OverlayRename
)

// Item is one row in an overlay.
type Item struct {
	Label  string
	Detail string
	// Value is what is inserted or acted on when the item is chosen.
	Value  string
	Status StatusKind
}

// Overlay is a floating list.
type Overlay struct {
	Kind  OverlayKind
	Title string
	// Trigger records the completion context an OverlayComplete came from, so
	// accepting an item replaces the right span of the prompt.
	Trigger Trigger

	items    []Item
	filtered []Item
	query    *Prompt
	sel      int
	// hover is the row under the pointer, or -1. It is drawn apart from the
	// selection: the pointer shows where a click would land, the selection is
	// what Enter would accept, and conflating them means the highlight jumps
	// away from a keyboard user's own navigation every time the mouse moves.
	hover int
	// listRect and listTop record where the last frame drew the item rows, so
	// a click or hover is tested against the geometry on screen rather than a
	// recomputed guess at it.
	listRect render.Rect
	listTop  int
	// Searchable gives the overlay its own query editor.
	Searchable bool
}

// NewOverlay builds a list overlay.
func NewOverlay(kind OverlayKind, title string, items []Item, searchable bool) *Overlay {
	o := &Overlay{Kind: kind, Title: title, items: items, Searchable: searchable, hover: -1}
	if searchable {
		o.query = NewPrompt()
		o.query.Placeholder = "type to filter"
	}
	o.refilter()
	return o
}

// Query is the overlay's own search editor, or nil when it has none.
//
// Nil for the inline completer specifically. Its query is the text already in
// the composer, and handing a second editor back would make typed characters
// land in the dropdown instead of the prompt they are meant to be filtering —
// which reads as the composer having stopped accepting input.
func (o *Overlay) Query() *Prompt {
	if !o.Searchable {
		return nil
	}
	return o.query
}

// SetFilter narrows the list to a query, used by the inline completer whose
// query lives in the main prompt rather than in the overlay.
func (o *Overlay) SetFilter(q string) {
	if o.query == nil {
		o.query = NewPrompt()
	}
	o.query.SetValue(q)
	o.refilter()
}

// Refilter recomputes the visible list after the query changed.
func (o *Overlay) Refilter() { o.refilter() }

func (o *Overlay) refilter() {
	q := ""
	if o.query != nil {
		q = strings.ToLower(o.query.Value())
	}
	if q == "" {
		o.filtered = o.items
		o.clampSel()
		return
	}
	// Two tiers: prefix matches first, then subsequence matches. Ranking
	// prefixes above scattered matches is what stops "/check" from being
	// outranked by an unrelated command that happens to contain the letters.
	var prefix, sub []Item
	for _, it := range o.items {
		label := strings.ToLower(it.Label)
		switch {
		case strings.HasPrefix(label, q):
			prefix = append(prefix, it)
		case subsequence(label, q) || strings.Contains(strings.ToLower(it.Detail), q):
			sub = append(sub, it)
		}
	}
	o.filtered = append(prefix, sub...)
	o.clampSel()
}

// subsequence reports whether pattern's characters appear in text in order.
//
// Both are walked as runes. Indexing the pattern by byte while comparing runes
// is the version that compiles and passes an ASCII test, then fails to match
// anything the moment a query contains a multi-byte character.
func subsequence(text, pattern string) bool {
	want := []rune(pattern)
	i := 0
	for _, r := range text {
		if i < len(want) && want[i] == r {
			i++
		}
	}
	return i == len(want)
}

func (o *Overlay) clampSel() {
	if o.sel >= len(o.filtered) {
		o.sel = len(o.filtered) - 1
	}
	if o.sel < 0 {
		o.sel = 0
	}
}

// Move changes the highlighted row.
func (o *Overlay) Move(delta int) {
	if len(o.filtered) == 0 {
		return
	}
	o.sel = (o.sel + delta + len(o.filtered)) % len(o.filtered)
}

// MoveTo puts the highlight on a specific row, for a pointer click.
func (o *Overlay) MoveTo(i int) {
	if i < 0 || i >= len(o.filtered) {
		return
	}
	o.sel = i
}

// Sel is the highlighted row's index.
func (o *Overlay) Sel() int { return o.sel }

// ItemAt maps a screen point to a filtered item's index, or -1 when the point
// is not on an item row — the border, the title, the query field.
func (o *Overlay) ItemAt(x, y int) int {
	if o.listRect.Empty() || !o.listRect.Contains(x, y) {
		return -1
	}
	idx := o.listTop + (y - o.listRect.Y)
	if idx < 0 || idx >= len(o.filtered) {
		return -1
	}
	return idx
}

// HoverAt moves the pointer highlight to the item under (x,y), reporting
// whether it moved. The caller repaints only on a true return, so a pointer
// crossing cells within one row costs no frame.
func (o *Overlay) HoverAt(x, y int) bool {
	idx := o.ItemAt(x, y)
	if idx == o.hover {
		return false
	}
	o.hover = idx
	return true
}

// Selected returns the highlighted item.
func (o *Overlay) Selected() (Item, bool) {
	if o.sel < 0 || o.sel >= len(o.filtered) {
		return Item{}, false
	}
	return o.filtered[o.sel], true
}

// Empty reports whether anything matched.
func (o *Overlay) Empty() bool { return len(o.filtered) == 0 }

// Height is the rows the overlay wants, bounded by max.
func (o *Overlay) Height(max int) int {
	h := len(o.filtered) + 2 // frame
	if o.Searchable {
		h += 2
	}
	if len(o.filtered) == 0 {
		h += 1
	}
	if h > max {
		h = max
	}
	if h < 3 {
		h = 3
	}
	return h
}

// Draw paints the overlay, returning the caret position when it is searchable.
func (o *Overlay) Draw(b *render.Buffer, r render.Rect, th Theme) render.Rect {
	fill := render.Style{Fg: th.Fg, Bg: th.BgOverlay}
	box := render.Box{
		Border: borderFor(th),
		Style:  render.Style{Fg: th.BorderFocus, Bg: th.BgOverlay},
		Title:  render.Styled(" "+o.Title+" ", render.Style{Fg: th.Accent, Bg: th.BgOverlay, Attrs: render.Bold}),
		Fill:   &fill,
	}
	inner := box.Draw(b, r)
	if inner.Empty() {
		return render.Rect{}
	}

	var caret render.Rect
	if o.Searchable && o.query != nil {
		field := render.Rect{X: inner.X + 1, Y: inner.Y, W: inner.W - 2, H: 1}
		b.Fill(field, ' ', render.Style{Fg: th.Fg, Bg: th.BgInset})
		caret = o.query.Draw(b, field, th, true)
		inner = inner.Pad(2, 0, 0, 0)
	}

	if len(o.filtered) == 0 {
		render.Styled("  no match", render.Style{Fg: th.FgSubtle, Bg: th.BgOverlay}).
			DrawIn(b, render.Rect{X: inner.X, Y: inner.Y, W: inner.W, H: 1})
		return caret
	}

	// The window follows the selection, so a long list stays navigable.
	top := 0
	if o.sel >= inner.H {
		top = o.sel - inner.H + 1
	}
	// Recorded for the pointer: a click is tested against the rows as they
	// were drawn, window offset included.
	o.listRect, o.listTop = inner, top
	for i := 0; i < inner.H && top+i < len(o.filtered); i++ {
		it := o.filtered[top+i]
		y := inner.Y + i
		selected := top+i == o.sel
		hovered := top+i == o.hover

		labelStyle := render.Style{Fg: th.Fg, Bg: th.BgOverlay}
		detailStyle := render.Style{Fg: th.FgSubtle, Bg: th.BgOverlay}
		if it.Status != StatusNeutral {
			labelStyle.Fg = th.Status(it.Status).Fg
		}
		if selected {
			labelStyle.Bg, detailStyle.Bg = th.Selection, th.Selection
			labelStyle.Attrs |= render.Bold
		} else if hovered {
			labelStyle.Bg, detailStyle.Bg = th.BgInset, th.BgInset
		}

		row := render.Rect{X: inner.X, Y: y, W: inner.W, H: 1}
		if selected {
			b.Fill(row, ' ', render.Style{Bg: th.Selection})
		} else if hovered {
			b.Fill(row, ' ', render.Style{Bg: th.BgInset})
		}
		line := render.Styled("  ", labelStyle).Append(it.Label, labelStyle)
		if it.Detail != "" {
			line = line.Append("  "+it.Detail, detailStyle)
		}
		line.Truncate(inner.W).Draw(b, inner.X, y)
	}
	return caret
}

// HelpOverlay renders the binding table, grouped by context.
//
// It is generated from the same table the dispatcher resolves against, so it
// cannot document a key the UI does not implement.
func HelpOverlay() *Overlay {
	var items []Item
	seen := map[Context]bool{}
	for _, ctx := range []Context{CtxGlobal, CtxPrompt, CtxScrollback, CtxDashboard, CtxApproval, CtxOverlay} {
		for _, b := range allBindings() {
			if b.Ctx != ctx || b.Label == "" {
				continue
			}
			if !seen[ctx] {
				items = append(items, Item{Label: "── " + ctxName(ctx) + " ──", Status: StatusInfo})
				seen[ctx] = true
			}
			items = append(items, Item{
				Label:  strings.Join(b.Keys, " / "),
				Detail: b.Label,
			})
		}
	}
	return NewOverlay(OverlayHelp, "keyboard shortcuts", items, true)
}

// SessionsOverlay renders the active sessions list for quick switching.
func SessionsOverlay(views []*AgentView, active int) *Overlay {
	var items []Item
	now := time.Now()
	for i, v := range views {
		status := StatusPass
		chip := "ready"
		switch {
		case v.Approval() != nil:
			status = StatusWarn
			chip = "approval needed"
		case v.Error() != "":
			status = StatusBlock
			chip = "error: " + v.Error()
		case v.Status.Busy:
			status = StatusInfo
			chip = "working: " + v.Status.BusyLabel
		}
		detail := chip
		if v.Status.Model != "" {
			detail += " • " + v.Status.Model
		}
		if v.Status.TaskID != "" {
			detail += " • task: " + v.Status.TaskID
		}
		if !v.Activity().IsZero() {
			detail += " • " + shortDuration(now.Sub(v.Activity())) + " ago"
		}
		marker := ""
		if i == active {
			marker = " (active)"
		}
		items = append(items, Item{
			Label:  fmt.Sprintf("[%d] %s%s", i+1, v.Title, marker),
			Detail: detail,
			Value:  v.ID,
			Status: status,
		})
	}
	return NewOverlay(OverlaySessions, "switch session", items, true)
}

// ThemeOverlay lists available color palettes.
func ThemeOverlay(current string) *Overlay {
	items := []Item{
		{Label: "Dark", Detail: "GitHub Dark palette with distinct status hues (default)", Value: "dark", Status: StatusPass},
		{Label: "Light", Detail: "High-contrast light background theme", Value: "light", Status: StatusInfo},
		{Label: "Plain", Detail: "Monochrome / NO_COLOR attribute-only fallback", Value: "plain", Status: StatusNeutral},
	}
	for i := range items {
		if strings.EqualFold(items[i].Value, current) {
			items[i].Label += " (active)"
		}
	}
	return NewOverlay(OverlayTheme, "color theme", items, true)
}

// SettingsOverlay renders every harness setting for browsing.
//
// This is the surface the flag report only looked like. `manvi flags` prints a
// table with a column headed by who may move each setting, and for as long as
// that table has existed there was no way to move one — the rows were text, the
// arrow keys moved between transcript entries rather than between rows, and the
// column said "human" to an operator with no human-authority path to reach.
//
// Choosing a row does not change anything. It writes `/flags set KEY ` into the
// composer with the legal values on the shortcut bar, so every move still goes
// through the one command that validates it, reports which direction a safety
// flag moved, and says how far the change reaches. A picker that flipped a
// safety flag on a single keypress would be a second way to change a setting,
// and the second way is always the one with no audit trail.
func SettingsOverlay(specs []SettingSpec) *Overlay {
	items := make([]Item, 0, len(specs))
	for _, s := range specs {
		status := StatusNeutral
		switch {
		case s.Safety && !s.AtSafest:
			// The one row an operator scanning this list must not miss.
			status = StatusWarn
		case s.Safety:
			status = StatusPass
		case s.Origin != "default":
			status = StatusInfo
		}

		label := s.Key
		if s.Safety {
			label = "! " + s.Key
		}
		detail := s.Value + "  (" + s.Origin + ")"
		if s.Mutable != "human" {
			// Said on the row rather than discovered by being refused. A
			// startup setting is not movable from here at all, and a picker
			// that armed a command certain to fail is a picker that wastes the
			// operator's keystroke and their trust in the list.
			detail += "  " + s.Mutable + "-only"
		}
		if s.Summary != "" {
			detail += "  — " + s.Summary
		}
		items = append(items, Item{Label: label, Detail: detail, Value: s.Key, Status: status})
	}
	return NewOverlay(OverlaySettings, "settings", items, true)
}

// SearchOverlay builds the transcript search prompt.
func SearchOverlay(initial string) *Overlay {
	o := NewOverlay(OverlaySearch, "search transcript", nil, true)
	if o.query != nil {
		o.query.Placeholder = "type query • enter jumps • n/N moves • esc closes"
		if initial != "" {
			o.query.SetValue(initial)
		}
	}
	return o
}

// RenameOverlay builds the session rename prompt.
func RenameOverlay(current string) *Overlay {
	o := NewOverlay(OverlayRename, "rename session", nil, true)
	if o.query != nil {
		o.query.Placeholder = "new session title"
		if current != "" {
			o.query.SetValue(current)
		}
	}
	return o
}
