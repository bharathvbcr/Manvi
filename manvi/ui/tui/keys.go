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
	{Keys: []string{"esc"}, Cmd: CmdClearDraft, Ctx: CtxPrompt, Label: "clear"},

	// Scrollback.
	{Keys: []string{"up", "k"}, Cmd: CmdSelectPrev, Ctx: CtxScrollback, Label: "prev entry", Hint: true},
	{Keys: []string{"down", "j"}, Cmd: CmdSelectNext, Ctx: CtxScrollback, Label: "next entry", Hint: true},
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

	// Dashboard.
	{Keys: []string{"up", "k"}, Cmd: CmdPrevItem, Ctx: CtxDashboard, Label: "prev", Hint: true},
	{Keys: []string{"down", "j"}, Cmd: CmdNextItem, Ctx: CtxDashboard, Label: "next", Hint: true},
	{Keys: []string{"enter"}, Cmd: CmdAccept, Ctx: CtxDashboard, Label: "open", Hint: true},
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
