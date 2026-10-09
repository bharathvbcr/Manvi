package tui

// Cmd is a bound command: what a key means once focus and mode are known.
type Cmd int

// The bound commands.
const (
	CmdNone Cmd = iota

	// Global.
	CmdQuit
	CmdPalette
	CmdHelp
	CmdNewSession
	CmdCloseSession
	CmdDashboard
	CmdNextSession
	CmdPrevSession
	CmdSessions
	CmdTheme
	CmdSuspend
	CmdRedraw

	// Session.
	CmdFocusToggle
	CmdCancel
	CmdSubmit
	CmdNewline
	CmdMultiline
	CmdHistoryPrev
	CmdHistoryNext

	// Scrollback.
	CmdScrollUp
	CmdScrollDown
	CmdPageUp
	CmdPageDown
	CmdHalfPageUp
	CmdHalfPageDown
	CmdTop
	CmdBottom
	CmdSelectNext
	CmdSelectPrev
	CmdToggleFold
	CmdFoldAll
	CmdExpandAll
	CmdCopy
	CmdSearch
	CmdFindNext
	CmdFindPrev

	// Prompt editing.
	CmdCursorLeft
	CmdCursorRight
	CmdWordLeft
	CmdWordRight
	CmdLineStart
	CmdLineEnd
	CmdBackspace
	CmdDelete
	CmdDeleteWord
	CmdDeleteToStart
	CmdDeleteToEnd
	CmdClearDraft
	CmdExpandPaste
	CmdYank
	CmdUndo

	// Dashboard.
	CmdRenameSession

	// Overlays.
	CmdAccept
	CmdComplete
	CmdDismiss
	CmdNextItem
	CmdPrevItem
	CmdToggleSelect
)

// Context is where a key is being pressed.
type Context int

// The contexts, most specific first. An overlay takes keys before the view
// behind it does; that ordering is what makes Escape close a modal rather than
// cancel the turn running behind it.
const (
	CtxOverlay Context = iota
	CtxApproval
	CtxPrompt
	CtxScrollback
	CtxDashboard
	CtxGlobal
)

// Binding maps keys to a command in a context.
type Binding struct {
	Keys  []string
	Cmd   Cmd
	Ctx   Context
	Label string
	// Hint places the binding on the contextual shortcut bar. Not every binding
	// earns a place there — a bar that lists everything is read by nobody.
	Hint bool
}

// bindings is the single source of truth.
//
// The shortcut bar, the help modal, and the command palette all read this table
// rather than carrying their own copies. A UI that documents a key in one place
// and implements it in another documents keys it does not have.
var bindings = []Binding{
	// Overlays first.
	{Keys: []string{"esc"}, Cmd: CmdDismiss, Ctx: CtxOverlay, Label: "close", Hint: true},
	{Keys: []string{"enter"}, Cmd: CmdAccept, Ctx: CtxOverlay, Label: "select", Hint: true},
	{Keys: []string{"down", "ctrl+n"}, Cmd: CmdNextItem, Ctx: CtxOverlay, Label: "next"},
	{Keys: []string{"up", "ctrl+p"}, Cmd: CmdPrevItem, Ctx: CtxOverlay, Label: "prev"},
	{Keys: []string{"pgdn"}, Cmd: CmdPageDown, Ctx: CtxOverlay, Label: "page down"},
	{Keys: []string{"pgup"}, Cmd: CmdPageUp, Ctx: CtxOverlay, Label: "page up"},
	{Keys: []string{"tab"}, Cmd: CmdComplete, Ctx: CtxOverlay, Label: "complete", Hint: true},
	{Keys: []string{"left"}, Cmd: CmdCursorLeft, Ctx: CtxOverlay},
	{Keys: []string{"right"}, Cmd: CmdCursorRight, Ctx: CtxOverlay},
	{Keys: []string{"alt+left", "alt+b"}, Cmd: CmdWordLeft, Ctx: CtxOverlay},
	{Keys: []string{"alt+right", "alt+f"}, Cmd: CmdWordRight, Ctx: CtxOverlay},
	{Keys: []string{"home", "ctrl+a"}, Cmd: CmdLineStart, Ctx: CtxOverlay},
	{Keys: []string{"end", "ctrl+e"}, Cmd: CmdLineEnd, Ctx: CtxOverlay},
	{Keys: []string{"backspace"}, Cmd: CmdBackspace, Ctx: CtxOverlay},
	{Keys: []string{"delete", "ctrl+d"}, Cmd: CmdDelete, Ctx: CtxOverlay},
	{Keys: []string{"ctrl+w", "alt+backspace"}, Cmd: CmdDeleteWord, Ctx: CtxOverlay},
	{Keys: []string{"ctrl+u"}, Cmd: CmdDeleteToStart, Ctx: CtxOverlay},
	{Keys: []string{"ctrl+k"}, Cmd: CmdDeleteToEnd, Ctx: CtxOverlay},

	// An approval card is modal on purpose: it is a human-in-the-loop control,
	// and letting the keys behind it through is how one gets answered by
	// accident.
	{Keys: []string{"down", "j"}, Cmd: CmdNextItem, Ctx: CtxApproval, Label: "next option"},
	{Keys: []string{"up", "k"}, Cmd: CmdPrevItem, Ctx: CtxApproval, Label: "prev option"},
	{Keys: []string{"space"}, Cmd: CmdToggleSelect, Ctx: CtxApproval, Label: "tick option"},
	{Keys: []string{"enter"}, Cmd: CmdAccept, Ctx: CtxApproval, Label: "confirm", Hint: true},
	{Keys: []string{"esc"}, Cmd: CmdDismiss, Ctx: CtxApproval, Label: "deny", Hint: true},
	{Keys: []string{"left"}, Cmd: CmdCursorLeft, Ctx: CtxApproval},
	{Keys: []string{"right"}, Cmd: CmdCursorRight, Ctx: CtxApproval},
	{Keys: []string{"alt+left", "alt+b"}, Cmd: CmdWordLeft, Ctx: CtxApproval},
	{Keys: []string{"alt+right", "alt+f"}, Cmd: CmdWordRight, Ctx: CtxApproval},
	{Keys: []string{"home", "ctrl+a"}, Cmd: CmdLineStart, Ctx: CtxApproval},
	{Keys: []string{"end", "ctrl+e"}, Cmd: CmdLineEnd, Ctx: CtxApproval},
	{Keys: []string{"backspace"}, Cmd: CmdBackspace, Ctx: CtxApproval},
	{Keys: []string{"delete", "ctrl+d"}, Cmd: CmdDelete, Ctx: CtxApproval},
	{Keys: []string{"ctrl+w", "alt+backspace"}, Cmd: CmdDeleteWord, Ctx: CtxApproval},
	{Keys: []string{"ctrl+u"}, Cmd: CmdDeleteToStart, Ctx: CtxApproval},
	{Keys: []string{"ctrl+k"}, Cmd: CmdDeleteToEnd, Ctx: CtxApproval},

	// Prompt.
	{Keys: []string{"enter"}, Cmd: CmdSubmit, Ctx: CtxPrompt, Label: "send", Hint: true},
	{Keys: []string{"shift+enter", "alt+enter"}, Cmd: CmdNewline, Ctx: CtxPrompt, Label: "newline"},
	{Keys: []string{"ctrl+m"}, Cmd: CmdMultiline, Ctx: CtxPrompt, Label: "multiline"},
	{Keys: []string{"up"}, Cmd: CmdHistoryPrev, Ctx: CtxPrompt, Label: "history"},
	{Keys: []string{"down"}, Cmd: CmdHistoryNext, Ctx: CtxPrompt, Label: "history"},
	{Keys: []string{"left"}, Cmd: CmdCursorLeft, Ctx: CtxPrompt},
	{Keys: []string{"right"}, Cmd: CmdCursorRight, Ctx: CtxPrompt},
	{Keys: []string{"alt+left", "alt+b"}, Cmd: CmdWordLeft, Ctx: CtxPrompt},
	{Keys: []string{"alt+right", "alt+f"}, Cmd: CmdWordRight, Ctx: CtxPrompt},
	{Keys: []string{"home", "ctrl+a"}, Cmd: CmdLineStart, Ctx: CtxPrompt},
	{Keys: []string{"end", "ctrl+e"}, Cmd: CmdLineEnd, Ctx: CtxPrompt},
	{Keys: []string{"backspace"}, Cmd: CmdBackspace, Ctx: CtxPrompt},
	{Keys: []string{"delete", "ctrl+d"}, Cmd: CmdDelete, Ctx: CtxPrompt},
	{Keys: []string{"ctrl+w", "alt+backspace"}, Cmd: CmdDeleteWord, Ctx: CtxPrompt},
	{Keys: []string{"ctrl+u"}, Cmd: CmdDeleteToStart, Ctx: CtxPrompt},
	{Keys: []string{"ctrl+k"}, Cmd: CmdDeleteToEnd, Ctx: CtxPrompt},
	{Keys: []string{"ctrl+y"}, Cmd: CmdYank, Ctx: CtxPrompt, Label: "yank"},
	{Keys: []string{"ctrl+z", "ctrl+_"}, Cmd: CmdUndo, Ctx: CtxPrompt, Label: "undo"},
	{Keys: []string{"ctrl+o"}, Cmd: CmdExpandPaste, Ctx: CtxPrompt, Label: "expand paste"},
	{Keys: []string{"esc"}, Cmd: CmdClearDraft, Ctx: CtxPrompt, Label: "clear"},

	// Scrollback.
	{Keys: []string{"up", "k"}, Cmd: CmdSelectPrev, Ctx: CtxScrollback, Label: "prev entry", Hint: true},
	{Keys: []string{"down", "j"}, Cmd: CmdSelectNext, Ctx: CtxScrollback, Label: "next entry", Hint: true},
	{Keys: []string{"ctrl+f", "/"}, Cmd: CmdSearch, Ctx: CtxScrollback, Label: "search", Hint: true},
	{Keys: []string{"n"}, Cmd: CmdFindNext, Ctx: CtxScrollback, Label: "next match"},
	{Keys: []string{"N"}, Cmd: CmdFindPrev, Ctx: CtxScrollback, Label: "prev match"},
	{Keys: []string{"ctrl+k"}, Cmd: CmdScrollUp, Ctx: CtxScrollback},
	{Keys: []string{"ctrl+j"}, Cmd: CmdScrollDown, Ctx: CtxScrollback},
	{Keys: []string{"pgup"}, Cmd: CmdPageUp, Ctx: CtxScrollback},
	{Keys: []string{"pgdn"}, Cmd: CmdPageDown, Ctx: CtxScrollback},
	{Keys: []string{"ctrl+u"}, Cmd: CmdHalfPageUp, Ctx: CtxScrollback},
	{Keys: []string{"ctrl+d"}, Cmd: CmdHalfPageDown, Ctx: CtxScrollback},
	{Keys: []string{"g", "home"}, Cmd: CmdTop, Ctx: CtxScrollback, Label: "top"},
	{Keys: []string{"G", "end"}, Cmd: CmdBottom, Ctx: CtxScrollback, Label: "bottom"},
	{Keys: []string{"e", "enter"}, Cmd: CmdToggleFold, Ctx: CtxScrollback, Label: "fold", Hint: true},
	{Keys: []string{"E"}, Cmd: CmdFoldAll, Ctx: CtxScrollback, Label: "fold all"},
	{Keys: []string{"R"}, Cmd: CmdExpandAll, Ctx: CtxScrollback, Label: "expand all"},
	{Keys: []string{"y"}, Cmd: CmdCopy, Ctx: CtxScrollback, Label: "copy", Hint: true},
	{Keys: []string{"esc"}, Cmd: CmdDismiss, Ctx: CtxScrollback, Label: "back to prompt"},

	// Dashboard. Built for fan-out, so it moves in every way a list can:
	// one row, one page, or to an end.
	{Keys: []string{"up", "k", "left", "h"}, Cmd: CmdPrevItem, Ctx: CtxDashboard, Label: "prev", Hint: true},
	{Keys: []string{"down", "j", "right", "l"}, Cmd: CmdNextItem, Ctx: CtxDashboard, Label: "next", Hint: true},
	{Keys: []string{"pgup"}, Cmd: CmdPageUp, Ctx: CtxDashboard, Label: "page up"},
	{Keys: []string{"pgdn"}, Cmd: CmdPageDown, Ctx: CtxDashboard, Label: "page down"},
	{Keys: []string{"g", "home"}, Cmd: CmdTop, Ctx: CtxDashboard, Label: "first"},
	{Keys: []string{"G", "end"}, Cmd: CmdBottom, Ctx: CtxDashboard, Label: "last"},
	{Keys: []string{"enter"}, Cmd: CmdAccept, Ctx: CtxDashboard, Label: "open", Hint: true},
	{Keys: []string{"r"}, Cmd: CmdRenameSession, Ctx: CtxDashboard, Label: "rename", Hint: true},
	{Keys: []string{"ctrl+x"}, Cmd: CmdCloseSession, Ctx: CtxDashboard, Label: "close session"},
	{Keys: []string{"esc"}, Cmd: CmdDismiss, Ctx: CtxDashboard, Label: "back", Hint: true},

	// Global. Ctrl+C is deliberately not quit: in raw mode it is the only way
	// to interrupt a turn, and a harness that exits on it loses the transcript
	// of whatever the operator was trying to stop.
	{Keys: []string{"ctrl+c"}, Cmd: CmdCancel, Ctx: CtxGlobal, Label: "cancel turn", Hint: true},
	{Keys: []string{"tab"}, Cmd: CmdFocusToggle, Ctx: CtxGlobal, Label: "focus", Hint: true},
	{Keys: []string{"ctrl+p"}, Cmd: CmdPalette, Ctx: CtxGlobal, Label: "commands", Hint: true},
	{Keys: []string{"f1", "ctrl+x"}, Cmd: CmdHelp, Ctx: CtxGlobal, Label: "keys"},
	{Keys: []string{"ctrl+q", "ctrl+d"}, Cmd: CmdQuit, Ctx: CtxGlobal, Label: "quit", Hint: true},
	{Keys: []string{"ctrl+n"}, Cmd: CmdNewSession, Ctx: CtxGlobal, Label: "new session"},
	{Keys: []string{"ctrl+g"}, Cmd: CmdDashboard, Ctx: CtxGlobal, Label: "dashboard", Hint: true},
	{Keys: []string{"ctrl+t"}, Cmd: CmdNextSession, Ctx: CtxGlobal, Label: "next session"},
	// Shift+Tab is the one "previous" chord every terminal encodes (CSI Z);
	// ctrl+shift+t exists only on keyboards the wire protocol cannot see.
	// The command it binds existed from the start but had no key, so the
	// strip advertised "next" and offered no way back.
	{Keys: []string{"shift+tab"}, Cmd: CmdPrevSession, Ctx: CtxGlobal, Label: "prev session"},
	{Keys: []string{"ctrl+s"}, Cmd: CmdSessions, Ctx: CtxGlobal, Label: "sessions", Hint: true},
	{Keys: []string{"ctrl+y"}, Cmd: CmdTheme, Ctx: CtxGlobal, Label: "theme"},
	{Keys: []string{"ctrl+z"}, Cmd: CmdSuspend, Ctx: CtxGlobal, Label: "suspend"},
	{Keys: []string{"ctrl+l"}, Cmd: CmdRedraw, Ctx: CtxGlobal, Label: "redraw"},
}

// resolve looks a key up in the given context, falling back to global.
//
// The fallback is one level and deliberate: a key bound in the active context
// wins, and anything unbound there falls through to the global table. Without
// the fallback every context would have to redeclare Ctrl+C; with a deeper
// chain, a context could shadow a binding it never mentions.
func resolve(key string, ctx Context) Cmd {
	for _, b := range bindings {
		if b.Ctx != ctx {
			continue
		}
		for _, k := range b.Keys {
			if k == key {
				return b.Cmd
			}
		}
	}
	if ctx == CtxGlobal {
		return CmdNone
	}
	return resolve(key, CtxGlobal)
}

// hintsFor returns the shortcut-bar entries for a context, global ones last.
func hintsFor(ctx Context) []Binding {
	var out []Binding
	for _, b := range bindings {
		if b.Ctx == ctx && b.Hint {
			out = append(out, b)
		}
	}
	if ctx != CtxGlobal {
		for _, b := range bindings {
			if b.Ctx == CtxGlobal && b.Hint {
				out = append(out, b)
			}
		}
	}
	return out
}

// allBindings exposes the table for the help modal.
func allBindings() []Binding { return bindings }

// ctxName labels a context in the help modal.
func ctxName(c Context) string {
	switch c {
	case CtxOverlay:
		return "overlay"
	case CtxApproval:
		return "approval"
	case CtxPrompt:
		return "prompt"
	case CtxScrollback:
		return "transcript"
	case CtxDashboard:
		return "dashboard"
	}
	return "global"
}

func (a *App) rune(t ActionRune) []Effect {
	if len(t.Runes) == 0 {
		return nil
	}
	if p := a.textTarget(); p != nil {
		for _, r := range t.Runes {
			p.Insert(r)
		}
		if a.overlay != nil && a.overlay.Searchable {
			a.overlay.Refilter()
		} else {
			a.refreshCompletion()
		}
		return nil
	}
	// Outside a text field a printable key is a binding: j, k, g, y in the
	// transcript, digits on an approval card. Space arrives here as a rune
	// because it is printable, but the bindings table names it "space" — the
	// same name the input decoder gives it — so it is translated rather than
	// looked up as " " and found by nothing.
	if len(t.Runes) == 1 && t.Runes[0] == ' ' {
		return a.key("space")
	}
	return a.key(string(t.Runes))
}

func (a *App) key(binding string) []Effect {
	ctx := a.Context()

	// Digits pick an approval option directly, which is the fastest safe answer
	// and the one an operator reaches for under time pressure.
	if ctx == CtxApproval && len(binding) == 1 && binding[0] >= '1' && binding[0] <= '9' {
		if v := a.Current(); v != nil && v.Approval() != nil {
			card := v.Approval()
			idx := int(binding[0] - '1')
			if idx < len(card.options()) {
				card.option = idx
				// On a multi-select the digit ticks the row rather than
				// answering with it. Answering on the first digit would send a
				// one-option answer to a question that asked for several, and
				// the result could not be told from a deliberate single pick.
				if card.Toggle() {
					return nil
				}
				return a.acceptApproval()
			}
		}
	}

	switch resolve(binding, ctx) {
	case CmdQuit:
		return a.requestQuit()
	case CmdRedraw:
		return []Effect{EffectRedraw{}}
	case CmdSuspend:
		return []Effect{EffectSuspend{}}
	case CmdHelp:
		a.overlay = HelpOverlay()
		return nil
	case CmdPalette:
		a.openPalette()
		return nil
	case CmdDashboard:
		return a.toggleDashboard()
	case CmdRenameSession:
		current := ""
		if a.mode == ModeDashboard && len(a.views) > 0 {
			idx := a.dashboard.Selected()
			if idx >= 0 && idx < len(a.views) {
				current = a.views[idx].Title
			}
		} else if v := a.Current(); v != nil {
			current = v.Title
		}
		a.overlay = RenameOverlay(current)
		return nil
	case CmdNewSession:
		return []Effect{EffectNewSession{}}
	case CmdCloseSession:
		return a.closeSelected()
	case CmdNextSession:
		return a.cycleSession(1)
	case CmdPrevSession:
		return a.cycleSession(-1)
	case CmdFocusToggle:
		return a.toggleFocus()
	case CmdToggleSelect:
		if v := a.Current(); v != nil && v.Approval() != nil {
			v.Approval().Toggle()
		}
		return nil
	case CmdCancel:
		return a.cancel()
	case CmdSubmit:
		return a.submit()
	case CmdNewline:
		if p := a.textTarget(); p != nil {
			p.Insert('\n')
		}
		return nil
	case CmdMultiline:
		if v := a.Current(); v != nil {
			v.Prompt.Multiline = !v.Prompt.Multiline
			a.setNotice("multiline "+onOff(v.Prompt.Multiline)+" — enter "+
				map[bool]string{true: "inserts a newline", false: "sends"}[v.Prompt.Multiline], StatusInfo)
		}
		return nil
	case CmdAccept:
		return a.accept()
	case CmdComplete:
		return a.complete()
	case CmdDismiss:
		return a.dismiss()
	case CmdNextItem:
		a.moveSelection(1)
		return nil
	case CmdPrevItem:
		a.moveSelection(-1)
		return nil
	case CmdClearDraft:
		return a.clearDraft()
	case CmdCopy:
		if v := a.Current(); v != nil {
			if text := v.Scroll.SelectedText(); text != "" {
				a.setNotice("copied "+itoa(len(text))+" bytes", StatusInfo)
				return []Effect{EffectCopy{Text: text}}
			}
		}
		return nil
	case CmdHistoryPrev:
		return a.history(-1)
	case CmdHistoryNext:
		return a.history(1)
	case CmdPageUp:
		if a.overlay != nil {
			a.overlay.MovePage(-1)
			return nil
		}
		if ctx == CtxDashboard {
			a.dashboard.Page(-1, len(a.views))
			return nil
		}
	case CmdPageDown:
		if a.overlay != nil {
			a.overlay.MovePage(1)
			return nil
		}
		if ctx == CtxDashboard {
			a.dashboard.Page(1, len(a.views))
			return nil
		}
	case CmdTop:
		if ctx == CtxDashboard {
			a.dashboard.Select(0)
			return nil
		}
	case CmdBottom:
		if ctx == CtxDashboard {
			a.dashboard.Select(len(a.views) - 1)
			a.dashboard.Clamp(len(a.views))
			return nil
		}
		// No return outside the handled surfaces: the transcript's own
		// pgup/pgdn/g/G resolve to the same commands and are answered by
		// scrollbackKey below.
	}

	if v := a.Current(); v != nil {
		if handled := a.scrollbackKey(v, binding, ctx); handled {
			return nil
		}
	}
	if p := a.textTarget(); p != nil && a.promptKey(p, binding, ctx) {
		if a.overlay != nil && a.overlay.Searchable {
			a.overlay.Refilter()
		} else {
			a.refreshCompletion()
		}
		return nil
	}
	return nil
}

func onOff(v bool) string {
	if v {
		return "on"
	}
	return "off"
}

func (a *App) scrollbackKey(v *AgentView, binding string, ctx Context) bool {
	if ctx != CtxScrollback {
		return false
	}
	sb := v.Scroll
	// The page size is the transcript's own height from the last frame, not a
	// figure derived from the terminal's — those differ by the status bar, the
	// strict banner, the composer, and whatever else the layout placed.
	page := sb.Viewport()
	switch resolve(binding, CtxScrollback) {
	case CmdScrollUp:
		sb.ScrollBy(-1)
	case CmdScrollDown:
		sb.ScrollBy(1)
	case CmdPageUp:
		sb.ScrollBy(-page)
	case CmdPageDown:
		sb.ScrollBy(page)
	case CmdHalfPageUp:
		sb.ScrollBy(-page / 2)
	case CmdHalfPageDown:
		sb.ScrollBy(page / 2)
	case CmdTop:
		sb.ScrollToTop()
	case CmdBottom:
		sb.ScrollToBottom()
	case CmdSelectNext:
		sb.SelectDelta(1, a.Theme)
	case CmdSelectPrev:
		sb.SelectDelta(-1, a.Theme)
	case CmdToggleFold:
		sb.ToggleFold()
	case CmdFoldAll:
		sb.SetFoldAll(true)
	case CmdExpandAll:
		sb.SetFoldAll(false)
	case CmdSearch:
		query := ""
		if sb.SearchQuery() != "" {
			query = sb.SearchQuery()
		}
		a.overlay = SearchOverlay(query)
	case CmdFindNext:
		if sb.NextMatch(a.Theme) {
			a.setNotice(sb.SearchStatus(), StatusInfo)
		} else if sb.SearchQuery() != "" {
			a.setNotice("no matches for: "+sb.SearchQuery(), StatusWarn)
		} else {
			a.setNotice("no active search — press ctrl+f to search", StatusInfo)
		}
	case CmdFindPrev:
		if sb.PrevMatch(a.Theme) {
			a.setNotice(sb.SearchStatus(), StatusInfo)
		} else if sb.SearchQuery() != "" {
			a.setNotice("no matches for: "+sb.SearchQuery(), StatusWarn)
		} else {
			a.setNotice("no active search — press ctrl+f to search", StatusInfo)
		}
	default:
		return false
	}
	return true
}

func (a *App) promptKey(p *Prompt, binding string, ctx Context) bool {
	cmd := resolve(binding, ctx)
	if cmd == CmdNone {
		cmd = resolve(binding, CtxPrompt)
	}
	switch cmd {
	case CmdCursorLeft:
		p.MoveLeft()
	case CmdCursorRight:
		p.MoveRight()
	case CmdWordLeft:
		p.MoveWordLeft()
	case CmdWordRight:
		p.MoveWordRight()
	case CmdLineStart:
		p.MoveHome()
	case CmdLineEnd:
		p.MoveEnd()
	case CmdBackspace:
		p.Backspace()
	case CmdDelete:
		p.Delete()
	case CmdDeleteWord:
		p.DeleteWord()
	case CmdDeleteToStart:
		p.DeleteToStart()
	case CmdDeleteToEnd:
		p.DeleteToEnd()
	case CmdYank:
		p.Yank()
	case CmdUndo:
		p.Undo()
	case CmdExpandPaste:
		if p.ExpandPastes() {
			a.setNotice("expanded paste", StatusInfo)
		}
	default:
		return false
	}
	return true
}

func (a *App) history(delta int) []Effect {
	v := a.Current()
	if v == nil || a.overlay != nil || v.Approval() != nil || v.Focus != CtxPrompt {
		return nil
	}
	if delta < 0 {
		v.Prompt.HistoryPrev()
	} else {
		v.Prompt.HistoryNext()
	}
	return nil
}

func (a *App) toggleFocus() []Effect {
	v := a.Current()
	if v == nil || a.mode != ModeSession {
		return nil
	}
	if v.Focus == CtxPrompt {
		v.Focus = CtxScrollback
		// Entering the transcript with nothing selected selects the newest
		// entry, so the first arrow key moves rather than jumping to the top.
		v.Scroll.SelectDelta(0, a.Theme)
	} else {
		v.Focus = CtxPrompt
		v.Scroll.SelectNone()
	}
	return nil
}

func (a *App) toggleDashboard() []Effect {
	if a.mode == ModeDashboard {
		a.mode = ModeSession
		return nil
	}
	a.mode = ModeDashboard
	a.dashboard.Select(a.active)
	a.dashboard.Clamp(len(a.views))
	return nil
}

func (a *App) cycleSession(delta int) []Effect {
	if len(a.views) == 0 {
		return nil
	}
	a.active = (a.active + delta + len(a.views)) % len(a.views)
	a.mode = ModeSession
	return nil
}

func (a *App) moveSelection(delta int) {
	if a.overlay != nil {
		a.overlay.Move(delta)
		return
	}
	if a.mode == ModeDashboard {
		a.dashboard.Move(delta, len(a.views))
		return
	}
	if v := a.Current(); v != nil && v.Approval() != nil {
		v.Approval().Next(delta)
	}
}

// complete is Tab inside an overlay: take the highlighted row.
func (a *App) complete() []Effect {
	if a.overlay == nil {
		return nil
	}
	return a.acceptOverlay()
}
