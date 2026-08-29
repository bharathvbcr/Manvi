package tui

import (
	"fmt"
	"strings"
	"unicode"

	"manvi/ui/render"
)

// Prompt is the composer: a text editor with history, word motion, and the
// trigger detection that drives slash-command and file completion.
//
// The buffer is []rune rather than string. Every operation an editor performs —
// cursor motion, backspace, word jumps — is defined in characters, and doing
// them on a string means converting byte offsets to rune offsets at every step,
// which is where editors acquire the bug that Backspace over an accented
// character deletes half of it.
type Prompt struct {
	runes  []rune
	cursor int

	// history is previous submissions, oldest first.
	history []string
	// histIdx walks history; len(history) means "not browsing".
	histIdx int
	// draft holds what was being typed before history browsing started, so
	// walking off the end of history restores it rather than clearing it.
	draft string

	// killRing holds text removed by delete-word / delete-line operations.
	killRing []string
	// undoStack holds previous buffer snapshots for Ctrl+Z / undo.
	undoStack [][]rune
	// pastes tracks collapsed paste chips and their original text.
	pastes map[string]string

	// Multiline makes Enter insert a newline instead of submitting.
	Multiline bool
	// Placeholder is shown when the buffer is empty.
	Placeholder string
}

// Rotating placeholder tips for feature discovery.
var rotatingTips = []string{
	"ask, or / for commands",
	"@ to attach file context",
	"Tab switches to transcript • Ctrl+F searches",
	"Ctrl+G opens dashboard",
	"Ctrl+P opens command palette",
}

// Tip returns a discoverability hint rotating on session activity.
func Tip(index int) string {
	if len(rotatingTips) == 0 {
		return "ask, or / for commands"
	}
	return rotatingTips[(index%len(rotatingTips)+len(rotatingTips))%len(rotatingTips)]
}

// NewPrompt builds an empty composer.
func NewPrompt() *Prompt {
	return &Prompt{Placeholder: "ask, or / for commands"}
}

// Value is the current text, with any collapsed paste chips expanded.
func (p *Prompt) Value() string {
	s := string(p.runes)
	if len(p.pastes) > 0 {
		for chip, full := range p.pastes {
			s = strings.ReplaceAll(s, chip, full)
		}
	}
	return s
}

// RawValue returns the literal runes in the buffer without expanding paste chips.
func (p *Prompt) RawValue() string { return string(p.runes) }

// InsertPaste inserts text, collapsing large pastes (>200 chars) into a compact chip.
func (p *Prompt) InsertPaste(s string) string {
	if len(s) <= 200 {
		p.InsertString(s)
		return ""
	}
	if p.pastes == nil {
		p.pastes = make(map[string]string)
	}
	chip := fmt.Sprintf("[pasted %d chars]", len(s))
	if _, exists := p.pastes[chip]; exists {
		chip = fmt.Sprintf("[pasted %d chars #%d]", len(s), len(p.pastes)+1)
	}
	p.pastes[chip] = s
	p.InsertString(chip)
	return chip
}

// ExpandPastes expands all collapsed paste chips in the buffer into their raw text.
func (p *Prompt) ExpandPastes() bool {
	if len(p.pastes) == 0 {
		return false
	}
	p.saveUndo()
	s := string(p.runes)
	expanded := false
	for chip, full := range p.pastes {
		if strings.Contains(s, chip) {
			s = strings.ReplaceAll(s, chip, full)
			expanded = true
		}
	}
	if expanded {
		p.runes = []rune(s)
		p.cursor = len(p.runes)
		p.pastes = nil
		return true
	}
	return false
}

// Empty reports whether anything has been typed.
func (p *Prompt) Empty() bool { return len(p.runes) == 0 }

// SetValue replaces the buffer and puts the cursor at the end.
func (p *Prompt) SetValue(s string) {
	p.saveUndo()
	p.runes = []rune(s)
	p.cursor = len(p.runes)
}

// Clear empties the buffer and stops history browsing.
func (p *Prompt) Clear() {
	p.saveUndo()
	p.runes = p.runes[:0]
	p.cursor = 0
	p.histIdx = len(p.history)
	p.draft = ""
}

func (p *Prompt) saveUndo() {
	if len(p.runes) == 0 && len(p.undoStack) == 0 {
		return
	}
	snapshot := append([]rune(nil), p.runes...)
	p.undoStack = append(p.undoStack, snapshot)
	if len(p.undoStack) > 50 {
		p.undoStack = p.undoStack[len(p.undoStack)-50:]
	}
}

func (p *Prompt) pushKill(s string) {
	if s == "" {
		return
	}
	p.killRing = append(p.killRing, s)
	if len(p.killRing) > 32 {
		p.killRing = p.killRing[len(p.killRing)-32:]
	}
}

// Undo restores the previous buffer state.
func (p *Prompt) Undo() bool {
	if len(p.undoStack) == 0 {
		return false
	}
	prev := p.undoStack[len(p.undoStack)-1]
	p.undoStack = p.undoStack[:len(p.undoStack)-1]
	p.runes = append([]rune(nil), prev...)
	if p.cursor > len(p.runes) {
		p.cursor = len(p.runes)
	}
	return true
}

// Yank inserts the most recently killed text at the cursor.
func (p *Prompt) Yank() bool {
	if len(p.killRing) == 0 {
		return false
	}
	text := p.killRing[len(p.killRing)-1]
	p.InsertString(text)
	return true
}

// KillRing exposes killed text fragments for testing.
func (p *Prompt) KillRing() []string { return p.killRing }

// Insert adds a rune at the cursor.
func (p *Prompt) Insert(r rune) {
	p.saveUndo()
	p.runes = append(p.runes, 0)
	copy(p.runes[p.cursor+1:], p.runes[p.cursor:])
	p.runes[p.cursor] = r
	p.cursor++
}

// InsertString adds text at the cursor, which is how a paste lands.
func (p *Prompt) InsertString(s string) {
	if s == "" {
		return
	}
	p.saveUndo()
	rs := []rune(s)
	p.runes = append(p.runes, rs...)
	copy(p.runes[p.cursor+len(rs):], p.runes[p.cursor:len(p.runes)-len(rs)])
	copy(p.runes[p.cursor:], rs)
	p.cursor += len(rs)
}

// Backspace deletes the rune before the cursor.
func (p *Prompt) Backspace() {
	if p.cursor == 0 {
		return
	}
	p.saveUndo()
	p.runes = append(p.runes[:p.cursor-1], p.runes[p.cursor:]...)
	p.cursor--
}

// Delete removes the rune at the cursor.
func (p *Prompt) Delete() {
	if p.cursor >= len(p.runes) {
		return
	}
	p.saveUndo()
	p.runes = append(p.runes[:p.cursor], p.runes[p.cursor+1:]...)
}

// DeleteWord removes the word before the cursor — Ctrl+W.
func (p *Prompt) DeleteWord() {
	start := p.wordStart()
	if start == p.cursor {
		return
	}
	p.saveUndo()
	p.pushKill(string(p.runes[start:p.cursor]))
	p.runes = append(p.runes[:start], p.runes[p.cursor:]...)
	p.cursor = start
}

// DeleteToStart removes everything before the cursor on this line — Ctrl+U.
func (p *Prompt) DeleteToStart() {
	start := p.lineStart()
	if start == p.cursor {
		return
	}
	p.saveUndo()
	p.pushKill(string(p.runes[start:p.cursor]))
	p.runes = append(p.runes[:start], p.runes[p.cursor:]...)
	p.cursor = start
}

// DeleteToEnd removes everything after the cursor on this line — Ctrl+K.
func (p *Prompt) DeleteToEnd() {
	end := p.lineEnd()
	if end == p.cursor {
		return
	}
	p.saveUndo()
	p.pushKill(string(p.runes[p.cursor:end]))
	p.runes = append(p.runes[:p.cursor], p.runes[end:]...)
}

// MoveLeft steps back one rune.
func (p *Prompt) MoveLeft() {
	if p.cursor > 0 {
		p.cursor--
	}
}

// SetCursor places the caret at a buffer index, clamped. Clicking in the
// composer lands here; it also ends history browsing, exactly as typing
// would, because a caret that moved under the pointer is no longer a draft
// being browsed.
func (p *Prompt) SetCursor(i int) {
	if i < 0 {
		i = 0
	}
	if i > len(p.runes) {
		i = len(p.runes)
	}
	p.cursor = i
	p.histIdx = len(p.history)
}

// IndexAt maps a (row, col) in the wrapped layout back to a buffer index —
// the inverse of layout, so a click in the composer puts the caret where it
// landed rather than at the nearest end.
//
// A click past a line's end lands at that line's end rather than wrapping to
// the next row's start: that is where the pointer is, and a caret that jumps
// to a different visual row than the one clicked reads as a miss.
func (p *Prompt) IndexAt(width, row, col int) int {
	if width < 1 {
		width = 1
	}
	if row < 0 || col < 0 {
		return 0
	}
	r, c := 0, 0
	for i := 0; i <= len(p.runes); i++ {
		// The caret sits before rune i, at column c of row r.
		if r == row && c >= col {
			return i
		}
		if i == len(p.runes) {
			break
		}
		rn := p.runes[i]
		if rn == '\n' {
			if r == row {
				return i
			}
			r++
			c = 0
			continue
		}
		w := render.RuneWidth(rn)
		if c+w > width {
			if r == row {
				return i
			}
			r++
			c = 0
		}
		c += w
	}
	return len(p.runes)
}

// Autosuggestion returns the ghost text matching history for the current prefix.
func (p *Prompt) Autosuggestion() string {
	if len(p.runes) == 0 || p.histIdx < len(p.history) {
		return ""
	}
	cur := string(p.runes)
	for i := len(p.history) - 1; i >= 0; i-- {
		h := p.history[i]
		if strings.HasPrefix(h, cur) && len(h) > len(cur) {
			return h[len(cur):]
		}
	}
	return ""
}

// AcceptAutosuggestion accepts any visible history suggestion.
func (p *Prompt) AcceptAutosuggestion() bool {
	sug := p.Autosuggestion()
	if sug == "" {
		return false
	}
	p.InsertString(sug)
	return true
}

// MoveRight steps forward one rune, or accepts ghost autosuggestion at line end.
func (p *Prompt) MoveRight() {
	if p.cursor < len(p.runes) {
		p.cursor++
	} else if p.cursor == len(p.runes) {
		p.AcceptAutosuggestion()
	}
}

// MoveWordLeft jumps to the start of the previous word.
func (p *Prompt) MoveWordLeft() { p.cursor = p.wordStart() }

// MoveWordRight jumps past the end of the next word.
func (p *Prompt) MoveWordRight() {
	i := p.cursor
	for i < len(p.runes) && isWordBoundary(p.runes[i]) {
		i++
	}
	for i < len(p.runes) && !isWordBoundary(p.runes[i]) {
		i++
	}
	p.cursor = i
}

// MoveHome goes to the start of the current line.
func (p *Prompt) MoveHome() { p.cursor = p.lineStart() }

// MoveEnd goes to the end of the current line, or accepts autosuggestion at line end.
func (p *Prompt) MoveEnd() {
	if p.cursor == p.lineEnd() && p.cursor == len(p.runes) && p.Autosuggestion() != "" {
		p.AcceptAutosuggestion()
		return
	}
	p.cursor = p.lineEnd()
}

// MoveStart goes to the start of the whole buffer.
func (p *Prompt) MoveStart() { p.cursor = 0 }

// MoveBufferEnd goes to the end of the whole buffer.
func (p *Prompt) MoveBufferEnd() { p.cursor = len(p.runes) }

func (p *Prompt) wordStart() int {
	i := p.cursor
	for i > 0 && isWordBoundary(p.runes[i-1]) {
		i--
	}
	for i > 0 && !isWordBoundary(p.runes[i-1]) {
		i--
	}
	return i
}

func (p *Prompt) lineStart() int {
	for i := p.cursor - 1; i >= 0; i-- {
		if p.runes[i] == '\n' {
			return i + 1
		}
	}
	return 0
}

func (p *Prompt) lineEnd() int {
	for i := p.cursor; i < len(p.runes); i++ {
		if p.runes[i] == '\n' {
			return i
		}
	}
	return len(p.runes)
}

func isWordBoundary(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("/\\.,;:()[]{}\"'`", r)
}

// Submit records the text in history and clears the buffer.
func (p *Prompt) Submit() string {
	text := strings.TrimRight(p.Value(), " \t\n")
	if text == "" {
		p.Clear()
		return ""
	}
	// Consecutive duplicates are not recorded. History exists to recall
	// something typed a while ago, and a run of identical retries buries it.
	if len(p.history) == 0 || p.history[len(p.history)-1] != text {
		p.history = append(p.history, text)
	}
	p.Clear()
	return text
}

// HistoryPrev walks backward through submissions.
func (p *Prompt) HistoryPrev() bool {
	if len(p.history) == 0 || p.histIdx == 0 {
		return false
	}
	if p.histIdx == len(p.history) {
		p.draft = p.Value()
	}
	p.histIdx--
	p.SetValue(p.history[p.histIdx])
	return true
}

// HistoryNext walks forward, restoring the draft at the end.
func (p *Prompt) HistoryNext() bool {
	if p.histIdx >= len(p.history) {
		return false
	}
	p.histIdx++
	if p.histIdx == len(p.history) {
		p.SetValue(p.draft)
		return true
	}
	p.SetValue(p.history[p.histIdx])
	return true
}

// History exposes the recorded submissions for the history panel.
func (p *Prompt) History() []string { return p.history }

// Trigger describes an in-progress completion.
type Trigger struct {
	// Kind is '/' for a slash command or '@' for a file reference; zero for none.
	Kind rune
	// Query is the text typed after the trigger.
	Query string
	// Start is the rune offset of the trigger character.
	Start int
}

// ActiveTrigger reports whether the cursor sits inside a completion context.
//
// A slash command only counts at the very start of the buffer. Anywhere else a
// slash is a path separator, and popping a command menu in the middle of
// "src/main.go" is the behaviour that makes completion feel hostile.
func (p *Prompt) ActiveTrigger() Trigger {
	if len(p.runes) > 0 && p.runes[0] == '/' {
		upto := string(p.runes[1:p.cursor])
		if !strings.ContainsAny(upto, " \n") {
			return Trigger{Kind: '/', Query: upto, Start: 0}
		}
	}
	for i := p.cursor - 1; i >= 0; i-- {
		r := p.runes[i]
		if r == '@' {
			// The trigger must begin a word, or an email address opens a file
			// picker on every keystroke.
			if i == 0 || unicode.IsSpace(p.runes[i-1]) {
				return Trigger{Kind: '@', Query: string(p.runes[i+1 : p.cursor]), Start: i}
			}
			return Trigger{}
		}
		if unicode.IsSpace(r) {
			return Trigger{}
		}
	}
	return Trigger{}
}

// ApplyCompletion replaces the active trigger's text with the chosen value.
func (p *Prompt) ApplyCompletion(t Trigger, value string) {
	if t.Kind == 0 {
		return
	}
	head := append([]rune(nil), p.runes[:t.Start]...)
	tail := append([]rune(nil), p.runes[p.cursor:]...)
	inserted := []rune(string(t.Kind) + value + " ")
	p.runes = append(append(head, inserted...), tail...)
	p.cursor = t.Start + len(inserted)
}

// layout wraps the buffer to a width and reports where the cursor lands.
func (p *Prompt) layout(width int) (lines []string, curRow, curCol int) {
	if width < 1 {
		width = 1
	}
	row, col := 0, 0
	var cur strings.Builder
	flush := func() {
		lines = append(lines, cur.String())
		cur.Reset()
	}
	for i, r := range p.runes {
		if i == p.cursor {
			curRow, curCol = row, col
		}
		if r == '\n' {
			flush()
			row++
			col = 0
			continue
		}
		w := render.RuneWidth(r)
		if col+w > width {
			flush()
			row++
			col = 0
		}
		cur.WriteRune(r)
		col += w
	}
	if p.cursor >= len(p.runes) {
		curRow, curCol = row, col
	}
	flush()
	return lines, curRow, curCol
}

// Height is how many rows the buffer needs at a width, bounded by max.
func (p *Prompt) Height(width, max int) int {
	lines, _, _ := p.layout(width)
	h := len(lines)
	if h < 1 {
		h = 1
	}
	if h > max {
		h = max
	}
	return h
}

// Draw paints the composer and returns where the caret belongs.
func (p *Prompt) Draw(b *render.Buffer, r render.Rect, th Theme, focused bool) render.Rect {
	if r.Empty() {
		return render.Rect{}
	}
	b.Fill(r, ' ', th.Base())

	if len(p.runes) == 0 {
		st := th.Subtle()
		b.SetStringClipped(r.X, r.Y, p.Placeholder, r.W, st)
		return render.Rect{X: r.X, Y: r.Y, W: 1, H: 1}
	}

	lines, curRow, curCol := p.layout(r.W)
	// The view follows the caret when the buffer is taller than the box.
	top := 0
	if curRow >= r.H {
		top = curRow - r.H + 1
	}
	for i := 0; i < r.H && top+i < len(lines); i++ {
		b.SetString(r.X, r.Y+i, lines[top+i], th.Base())
	}
	if focused && p.cursor == len(p.runes) {
		if sug := p.Autosuggestion(); sug != "" && curRow >= top && curRow < top+r.H {
			gy := r.Y + curRow - top
			gx := r.X + curCol
			if gx < r.Right() {
				ghost := render.Styled(sug, th.Subtle())
				ghost.Truncate(r.Right()-gx).Draw(b, gx, gy)
			}
		}
	}
	if !focused {
		return render.Rect{}
	}
	y := r.Y + curRow - top
	if y < r.Y {
		y = r.Y
	}
	if y >= r.Bottom() {
		y = r.Bottom() - 1
	}
	x := r.X + curCol
	if x >= r.Right() {
		x = r.Right() - 1
	}
	return render.Rect{X: x, Y: y, W: 1, H: 1}
}
