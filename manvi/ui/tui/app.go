package tui

import (
	"strings"
	"time"

	"manvi/ui"
	"manvi/ui/fx"
	"manvi/ui/logo"
	"manvi/ui/render"
)

// Mode is which top-level surface is showing.
type Mode int

// The modes.
const (
	ModeSession Mode = iota
	ModeDashboard
)

// App is the AppView: every session, the global overlays, and the dispatcher.
//
// Dispatch is the only thing that changes state, and it returns Effects rather
// than performing them. That split is what makes the whole UI testable without a
// terminal — a test constructs an App, feeds it Actions, and asserts on the
// state and the Effects it asked for. Nothing in this file opens a file,
// reaches the network, or writes to a tty.
type App struct {
	Theme Theme
	// Profile and Unicode are what the terminal can actually do, which is not
	// the same question as what the current theme uses. See NewApp.
	Profile render.Profile
	Unicode bool
	Host    Host

	views  []*AgentView
	active int
	mode   Mode

	overlay   *Overlay
	dashboard Dashboard

	// quitArmed supports the double-press confirmation. A single keystroke that
	// discards a running turn and its transcript is too cheap.
	quitArmed time.Time
	quit      bool

	tick   int
	width  int
	height int

	// fx is the ambient-animation gate, checked through fxOn. Set from the
	// environment by the runner; left on in tests, where every effect is a
	// deterministic function of tick.
	fx bool

	// tabs and tabRow are the session strip as the last frame drew it, for
	// pointer hit-testing; overlayRect is the same record for an open
	// overlay. Geometry is recorded at draw time rather than recomputed at
	// click time — the recomputed copy is the one that goes stale.
	tabs        []TabRef
	tabRow      render.Rect
	overlayRect render.Rect

	// lastClick* support the double-click (fold toggle in the transcript);
	// dragBar records that the press began on the scrollbar, so motion events
	// drag the viewport until the release.
	lastClick  time.Time
	lastClickX int
	lastClickY int
	dragBar    bool

	// series is the recent output-tokens-per-tick history feeding the status
	// bar's sparkline; lastOut is the total the last sample was taken
	// against.
	series  *fx.Series
	lastOut int

	// notices are transient messages shown on the shortcut bar.
	notice     string
	noticeAt   time.Time
	noticeKind StatusKind

	// pending holds events that arrived for a session before it was
	// registered, and dropped counts what the bound below discarded.
	//
	// The race is structural rather than incidental: a host builds a session's
	// state and announces it in the same call, so its opening events — the
	// posture, the model, the weakened-settings notice — are in flight before
	// the view that would display them exists. Discarding those is not a
	// cosmetic loss; the weakened-settings notice is the one that decides
	// whether the run may be described as strict.
	pending map[string][]ui.Event
	dropped map[string]int
}

// Doublepress is the window for confirmations and the double-Escape binding.
const DoublePress = 800 * time.Millisecond

// NewApp builds the application state.
func NewApp(th Theme, host Host) *App {
	// Profile and Unicode default from the starting theme and are overridden by
	// the caller that actually asked the terminal. They are kept apart from
	// Theme.Profile because the plain theme overwrites that field with NoColor:
	// reading the capability back off the live theme would mean switching to
	// plain and back returned a dark theme with every colour stripped, and the
	// terminal would look broken with nothing to point at.
	return &App{
		Theme: th, Host: host, active: -1, Profile: th.Profile, Unicode: th.Unicode,
		fx: fx.Enabled(), series: fx.NewSeries(24),
	}
}

// fxOn reports whether ambient animation is on for the current frame. The
// plain theme reports NoColor and means it — an operator who chose it chose a
// UI without motion as well as without colour, so one gate covers both.
func (a *App) fxOn() bool {
	return a.fx && a.Theme.Profile != render.NoColor
}

// Quitting reports whether the loop should stop.
func (a *App) Quitting() bool { return a.quit }

// Views exposes the sessions, for the runner's teardown.
func (a *App) Views() []*AgentView { return a.views }

// Current is the active session, or nil.
func (a *App) Current() *AgentView {
	if a.active < 0 || a.active >= len(a.views) {
		return nil
	}
	return a.views[a.active]
}

// AddSession registers a session and makes it active.
func (a *App) AddSession(id, title string) *AgentView {
	v := NewAgentView(id, title)
	v.Status = Status{Sessions: len(a.views) + 1}
	a.views = append(a.views, v)
	a.active = len(a.views) - 1
	a.syncCounts()
	return v
}

// maxHeld bounds the per-session buffer. A session id that never registers
// would otherwise accumulate without limit.
const maxHeld = 512

// hold buffers an event for a session that does not exist yet.
func (a *App) hold(id string, e ui.Event) {
	if id == "" {
		return
	}
	if a.pending == nil {
		a.pending = map[string][]ui.Event{}
		a.dropped = map[string]int{}
	}
	if len(a.pending[id]) >= maxHeld {
		a.dropped[id]++
		return
	}
	a.pending[id] = append(a.pending[id], e)
}

// release replays whatever was held for a session that has now registered.
func (a *App) release(v *AgentView) {
	held := a.pending[v.ID]
	for _, e := range held {
		v.Apply(e)
	}
	delete(a.pending, v.ID)

	if n := a.dropped[v.ID]; n > 0 {
		// Reported, never silent. A transcript that begins mid-run while
		// looking complete is exactly the thing the rest of this harness
		// refuses to produce.
		v.Apply(ui.Event{
			Kind: ui.KindNotice, At: time.Now().UTC(),
			Text:     itoa(n) + " event(s) arrived before this session was ready and were discarded",
			Degraded: []string{"transcript is incomplete at its start"},
		})
		delete(a.dropped, v.ID)
	}
}

func (a *App) viewByID(id string) *AgentView {
	for _, v := range a.views {
		if v.ID == id {
			return v
		}
	}
	return nil
}

func (a *App) syncCounts() {
	for _, v := range a.views {
		v.Status.Sessions = len(a.views)
	}
}

// Context is where keys are currently being resolved.
func (a *App) Context() Context {
	if a.overlay != nil {
		return CtxOverlay
	}
	if v := a.Current(); v != nil && v.Approval() != nil && a.mode == ModeSession {
		return CtxApproval
	}
	if a.mode == ModeDashboard {
		return CtxDashboard
	}
	if v := a.Current(); v != nil {
		return v.Focus
	}
	return CtxGlobal
}

// textTarget is the editor that should receive printable characters, or nil.
func (a *App) textTarget() *Prompt {
	if a.overlay != nil {
		if q := a.overlay.Query(); q != nil {
			return q
		}
		// The inline completer has no editor of its own: it is anchored to the
		// composer and filters what is already being typed there, so keys keep
		// going to the composer underneath it. Every other overlay swallows
		// them.
		if a.overlay.Kind != OverlayComplete {
			return nil
		}
	}
	v := a.Current()
	if v == nil {
		return nil
	}
	if v.Approval() != nil {
		return v.Approval().Prompt()
	}
	if a.mode == ModeSession && v.Focus == CtxPrompt {
		return v.Prompt
	}
	return nil
}

// Dispatch folds an action into state and returns the effects to run.
func (a *App) Dispatch(act Action) []Effect {
	switch t := act.(type) {
	case ActionTick:
		a.tick++
		if a.notice != "" && time.Since(a.noticeAt) > 6*time.Second {
			a.notice = ""
		}
		// Sample the throughput series the status bar sparklines. The delta
		// over all sessions is deliberate: the question the strip answers is
		// "is anything producing", not which session is.
		total := 0
		for _, v := range a.views {
			total += v.Status.OutputTokens
		}
		a.series.Push(float64(total - a.lastOut))
		a.lastOut = total
		return nil

	case ActionResize:
		a.width, a.height = t.W, t.H
		return nil

	case ActionEvent:
		if v := a.viewByID(t.SessionID); v != nil {
			v.Apply(t.Event)
			return nil
		}
		a.hold(t.SessionID, t.Event)
		return nil

	case ActionSessionAdded:
		v := a.AddSession(t.ID, t.Title)
		a.release(v)
		return nil

	case actionRemoveSession:
		a.RemoveSession(t.ID)
		return nil

	case ActionTurnStarted:
		if v := a.viewByID(t.SessionID); v != nil {
			v.SetBusy(true, "working")
		}
		return nil

	case ActionTurnEnded:
		return a.turnEnded(t)

	case ActionApprovalRequest:
		return a.approvalRequested(t)

	case ActionNotice:
		a.setNotice(t.Text, t.Status)
		// A notice with no session — a captured stderr line, a failure to start
		// a session — still belongs in a transcript, or it survives only as a
		// message that fades off the shortcut bar.
		v := a.viewByID(t.SessionID)
		if v == nil {
			v = a.Current()
		}
		if v != nil {
			kind := ui.KindNotice
			if t.Status == StatusBlock {
				kind = ui.KindError
			}
			v.Apply(ui.Event{Kind: kind, At: time.Now().UTC(), Text: t.Text})
		}
		return nil

	case ActionPaste:
		if p := a.textTarget(); p != nil {
			if v := a.Current(); v != nil && p == v.Prompt && len(t.Text) > 200 {
				chip := p.InsertPaste(t.Text)
				a.setNotice("pasted "+itoa(len(t.Text))+" chars (Ctrl+O expands) "+chip, StatusInfo)
			} else {
				p.InsertString(t.Text)
			}
			if a.overlay != nil && a.overlay.Searchable {
				a.overlay.Refilter()
			} else {
				a.refreshCompletion()
			}
		}
		return nil

	case ActionRune:
		return a.rune(t)

	case ActionKey:
		return a.key(t.Binding)

	case ActionScroll:
		return a.scroll(t)

	case ActionClick:
		return a.click(t)

	case ActionMotion:
		return a.motion(t)

	case ActionRelease:
		// Whatever drag the press began ends here.
		a.dragBar = false
		return nil
	}
	return nil
}

func (a *App) turnEnded(t ActionTurnEnded) []Effect {
	v := a.viewByID(t.SessionID)
	if v == nil {
		return nil
	}
	v.SetBusy(false, "")
	if t.Err != nil {
		v.Apply(ui.Event{Kind: ui.KindError, At: time.Now().UTC(), Text: t.Err.Error()})
	}
	// A follow-up typed during the turn goes now. It is sent rather than
	// silently dropped, and it is sent one at a time so a queue of three does
	// not start three concurrent turns on one session.
	if next, ok := v.Dequeue(); ok {
		return []Effect{EffectSubmit{SessionID: v.ID, Text: next}}
	}
	return nil
}

func (a *App) approvalRequested(t ActionApprovalRequest) []Effect {
	v := a.viewByID(t.SessionID)
	if v == nil {
		// No view owns this request. Refusing is the only safe answer: an
		// approval nobody can see must not be granted by absence.
		return []Effect{denyEffect(t.Reply, "no session is attached to put this to an operator")}
	}
	v.PushApproval(NewApprovalCard(t.Request, t.Reply))
	v.Apply(ui.Event{
		Kind: ui.KindApproval, At: time.Now().UTC(),
		Text: t.Request.Reason, Rule: t.Request.Rule, Severity: t.Request.Severity,
		Path: t.Request.Path, TaskID: t.Request.TaskID,
		Grantable: t.Request.Grantable, ApprovalID: t.Request.ID,
	})
	// A blocked session pulls attention to itself, but never by hijacking the
	// screen: if the operator is reading another session, the dashboard's
	// blocked count and this session's chip say so, and they move when they
	// choose to.
	if a.mode == ModeSession && a.Current() != nil && a.Current().ID != t.SessionID {
		a.setNotice("session "+v.Title+" is blocked on an approval", StatusWarn)
	}
	return nil
}

// denyEffect builds a refusal.
//
// Every path that cannot put a request to a human ends here rather than
// defaulting to allow. An approval nobody could see must not be granted by
// absence — that is the one failure mode this seam is not allowed to have.
func denyEffect(reply chan ui.Decision, why string) Effect {
	return EffectDecide{Reply: reply, Decision: ui.Decision{Allow: false, Reason: why}}
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

// uiCommands are the commands this package answers itself, with no harness
// involved.
//
// They are listed rather than only handled because a command that appears in
// neither menu is a command an operator has to already know exists. /sessions
// and /theme were exactly that: implemented, reachable by typing the name in
// full, and absent from the palette and the completion dropdown that are the
// only two places anyone goes looking.
var uiCommands = []CommandSpec{
	{Name: "settings", Summary: "browse every setting, its value, and where the value came from"},
	{Name: "sessions", Summary: "switch between open sessions"},
	{Name: "theme", Summary: "choose a colour palette"},
	{Name: "help", Summary: "show keyboard shortcuts and help"},
	{Name: "clear", Summary: "clear this transcript"},
	{Name: "quit", Summary: "leave manvi"},
}

// menuCommands is what the palette and the completion dropdown list: the
// host's commands, plus the ones this package answers that the host does not
// already declare.
func (a *App) menuCommands() []CommandSpec {
	var specs []CommandSpec
	seen := map[string]bool{}
	if a.Host != nil {
		for _, spec := range a.Host.Commands() {
			specs = append(specs, spec)
			seen[spec.Name] = true
		}
	}
	for _, spec := range uiCommands {
		if !seen[spec.Name] {
			specs = append(specs, spec)
		}
	}
	return specs
}

// commandSpec finds a command's declared shape, or reports that neither the
// host nor this package offers it.
func (a *App) commandSpec(name string) (CommandSpec, bool) {
	for _, spec := range a.menuCommands() {
		if spec.Name == name {
			return spec, true
		}
	}
	return CommandSpec{}, false
}

// openSettings shows every harness setting as a list that can be navigated.
func (a *App) openSettings() []Effect {
	if a.Host == nil {
		return nil
	}
	specs := a.Host.Settings()
	if len(specs) == 0 {
		a.setNotice("this harness reports no settings", StatusWarn)
		return nil
	}
	a.overlay = SettingsOverlay(specs)
	return nil
}

// settingSpec finds a setting by key from the host's current report.
func (a *App) settingSpec(key string) (SettingSpec, bool) {
	if a.Host == nil {
		return SettingSpec{}, false
	}
	for _, s := range a.Host.Settings() {
		if s.Key == key {
			return s, true
		}
	}
	return SettingSpec{}, false
}

// takesArgs reports whether choosing this command from a menu should stop and
// let the operator type its arguments.
//
// Both menus used to run the chosen command with no arguments at all. The
// palette displayed the usage — "[--all]", "list|acquire|release",
// "PATH --reason TEXT" — in the same row it then discarded, so every option
// every command has was unreachable from the menus that advertised them.
func (a *App) takesArgs(name string) bool {
	spec, ok := a.commandSpec(name)
	return ok && spec.Args != ""
}

// accept is Enter in whatever currently owns it.
func (a *App) accept() []Effect {
	if a.overlay != nil {
		if a.overlay.Kind == OverlayComplete {
			o := a.overlay
			item, ok := o.Selected()
			if ok {
				if v := a.Current(); v != nil {
					v.Prompt.ApplyCompletion(o.Trigger, item.Value)
				}
			}
			a.overlay = nil
			if o.Trigger.Kind != '/' || !ok {
				return nil
			}
			if a.takesArgs(item.Value) {
				// Completed into the composer and left there. Sending here is
				// what made '/flags --all' unreachable: the dropdown's own
				// footer names the option, and Enter ran the command without it.
				spec, _ := a.commandSpec(item.Value)
				a.setNotice("/"+item.Value+" "+spec.Args+" — enter sends it", StatusInfo)
				return nil
			}
			return a.submit()
		}
		return a.acceptOverlay()
	}
	if a.mode == ModeDashboard {
		if len(a.views) > 0 {
			a.active = a.dashboard.Selected()
			a.mode = ModeSession
		}
		return nil
	}
	if v := a.Current(); v != nil && v.Approval() != nil {
		return a.acceptApproval()
	}
	return a.submit()
}

func (a *App) acceptApproval() []Effect {
	v := a.Current()
	if v == nil || v.Approval() == nil {
		return nil
	}
	card := v.Approval()
	// The stage before the keypress, so the warning below fires for an empty
	// submission and not merely for arriving at the field. Warning on arrival
	// reads as a complaint about something the operator has not done yet.
	was := card.stage
	needed := card.NeedsSelection()
	decision, done := card.Accept()
	if !done {
		switch {
		case was == stageReason && card.isQuestion() && trimSpace(card.reason.Value()) == "":
			a.setNotice("an empty answer is not an answer — type one, or press esc to leave it unanswered", StatusWarn)
		case was == stageReason && trimSpace(card.reason.Value()) == "":
			a.setNotice("a reason is required — a grant nobody can review later is not issued", StatusWarn)
		case needed:
			// Enter did nothing, and silence would read as a broken key rather
			// than as a question still waiting on an answer.
			a.setNotice("tick at least one option with space — the cursor is not a selection", StatusWarn)
		}
		return nil
	}
	v.PopApproval()
	v.Apply(ui.Event{
		Kind: ui.KindApprovalDone, At: time.Now().UTC(),
		ApprovalID: card.Request.ID, Rule: card.Request.Rule, Path: card.Request.Path,
		Text: decisionText(decision),
	})
	if card.Reply != nil {
		select {
		case card.Reply <- decision:
		default:
		}
	}
	return []Effect{EffectDecide{Reply: card.Reply, Decision: decision}}
}

func decisionText(d ui.Decision) string {
	// An answered question is not a grant, and the transcript must not read as
	// though the operator permitted something. Answered rather than Allow,
	// because a decision carrying no choice is not an answer whatever Allow says.
	if d.Answered() {
		if d.WriteIn != "" {
			return "answered: " + d.WriteIn
		}
		return "answered: " + strings.Join(d.Chosen, ", ")
	}
	if d.Allow {
		return "allowed: " + d.Reason
	}
	return "denied: " + d.Reason
}

func (a *App) acceptOverlay() []Effect {
	o := a.overlay
	if o == nil {
		return nil
	}
	if o.Kind == OverlaySearch {
		query := ""
		if o.Query() != nil {
			query = o.Query().Value()
		}
		a.overlay = nil
		if v := a.Current(); v != nil {
			v.Focus = CtxScrollback
			if query != "" {
				v.Scroll.SetSearch(query, a.Theme)
				if v.Scroll.MatchCount() > 0 {
					a.setNotice(v.Scroll.SearchStatus()+" (n/N jumps)", StatusInfo)
				} else {
					a.setNotice("no matches for: "+query, StatusWarn)
				}
			} else {
				v.Scroll.ClearSearch()
			}
		}
		return nil
	}
	if o.Kind == OverlayRename {
		newTitle := ""
		if o.Query() != nil {
			newTitle = strings.TrimSpace(o.Query().Value())
		}
		a.overlay = nil
		if newTitle != "" {
			if a.mode == ModeDashboard && len(a.views) > 0 {
				idx := a.dashboard.Selected()
				if idx >= 0 && idx < len(a.views) {
					a.views[idx].SetTitle(newTitle)
				}
			} else if v := a.Current(); v != nil {
				v.SetTitle(newTitle)
			}
			a.setNotice("session renamed to "+newTitle, StatusInfo)
		}
		return nil
	}
	item, ok := o.Selected()
	if !ok {
		return nil
	}
	switch o.Kind {
	case OverlayComplete:
		if v := a.Current(); v != nil {
			v.Prompt.ApplyCompletion(o.Trigger, item.Value)
		}
		a.overlay = nil
		return nil
	case OverlayPalette:
		a.overlay = nil
		if a.takesArgs(item.Value) {
			return a.armCommand(item.Value)
		}
		return a.runCommand(item.Value, "")
	case OverlaySettings:
		a.overlay = nil
		return a.armSetting(item.Value)
	case OverlaySessions:
		a.overlay = nil
		for i, v := range a.views {
			if v.ID == item.Value {
				a.active = i
				a.mode = ModeSession
			}
		}
		return nil
	case OverlayTheme:
		a.overlay = nil
		// Resolved against the terminal's capability, not the outgoing theme's.
		a.Theme = PickThemeByNameUnchecked(item.Value, a.Profile, a.Unicode)
		return nil
	case OverlayHelp:
		return nil
	}
	return nil
}

// dismiss is Escape, resolved against whatever is on top.
func (a *App) dismiss() []Effect {
	if a.overlay != nil {
		a.overlay = nil
		return nil
	}
	v := a.Current()
	if v != nil && v.Approval() != nil {
		card := v.Approval()
		decision, done := card.Back()
		if !done {
			return nil
		}
		v.PopApproval()
		v.Apply(ui.Event{
			Kind: ui.KindApprovalDone, At: time.Now().UTC(),
			ApprovalID: card.Request.ID, Rule: card.Request.Rule,
			Path: card.Request.Path, Text: decisionText(decision),
		})
		return []Effect{EffectDecide{Reply: card.Reply, Decision: decision}}
	}
	if a.mode == ModeDashboard {
		a.mode = ModeSession
		return nil
	}
	if v != nil && v.Focus == CtxScrollback {
		v.Focus = CtxPrompt
		v.Scroll.SelectNone()
		return nil
	}
	return a.cancel()
}

// cancel stops a running turn, or clears the draft when nothing runs.
func (a *App) cancel() []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	if v.Status.Busy {
		a.setNotice("cancelling — the draft is kept", StatusWarn)
		return []Effect{EffectCancel{SessionID: v.ID}}
	}
	return a.clearDraft()
}

func (a *App) clearDraft() []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	if v.Prompt.Empty() {
		return nil
	}
	// Discarding typed text on one keystroke loses work. The second press
	// inside the window confirms it.
	if v.EscapePressed(DoublePress) {
		v.Prompt.Clear()
		a.overlay = nil
		return nil
	}
	a.setNotice("press again to clear the draft", StatusInfo)
	return nil
}

func (a *App) requestQuit() []Effect {
	busy := 0
	for _, v := range a.views {
		if v.Status.Busy {
			busy++
		}
	}
	if !a.quitArmed.IsZero() && time.Since(a.quitArmed) < 2*time.Second {
		a.quit = true
		return []Effect{EffectQuit{}}
	}
	a.quitArmed = time.Now()
	if busy > 0 {
		a.setNotice("press again to quit — "+itoa(busy)+" turn(s) still running", StatusWarn)
	} else {
		a.setNotice("press again to quit", StatusInfo)
	}
	return nil
}

func (a *App) closeSelected() []Effect {
	if a.mode != ModeDashboard || len(a.views) == 0 {
		return nil
	}
	v := a.views[a.dashboard.Selected()]
	return []Effect{EffectCloseSession{SessionID: v.ID}}
}

// RemoveSession drops a session from the view after the host released it.
func (a *App) RemoveSession(id string) {
	for i, v := range a.views {
		if v.ID != id {
			continue
		}
		a.views = append(a.views[:i], a.views[i+1:]...)
		if a.active >= len(a.views) {
			a.active = len(a.views) - 1
		}
		a.dashboard.Clamp(len(a.views))
		a.syncCounts()
		return
	}
}

func (a *App) submit() []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	if v.Prompt.Multiline {
		v.Prompt.Insert('\n')
		return nil
	}
	text := v.Prompt.Submit()
	if text == "" {
		return nil
	}
	if strings.HasPrefix(text, "/") {
		name, args, _ := strings.Cut(strings.TrimPrefix(text, "/"), " ")
		return a.runCommand(name, args)
	}
	if v.Status.Busy {
		// Queued rather than started. Two concurrent turns on one session would
		// interleave their writes into one session log, and the log is the only
		// thing the model's history is projected from.
		v.Queue(text)
		a.setNotice("queued — sends when this turn ends", StatusInfo)
		return nil
	}
	return []Effect{EffectSubmit{SessionID: v.ID, Text: text}}
}

// armCommand puts a command that takes arguments into the composer instead of
// running it bare.
//
// It refuses to overwrite a draft. The palette is reachable with half a prompt
// already typed, and silently replacing that text with "/lease " would destroy
// work to save a keystroke — so the draft wins and the operator is told why.
func (a *App) armCommand(name string) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	spec, _ := a.commandSpec(name)
	if !v.Prompt.Empty() {
		a.setNotice("/"+name+" takes "+spec.Args+
			" — the composer has a draft, so it was not replaced; clear it with esc and choose again", StatusWarn)
		return nil
	}
	v.Focus = CtxPrompt
	v.Prompt.SetValue("/" + name + " ")
	a.setNotice("/"+name+" "+spec.Args+" — enter sends it", StatusInfo)
	return nil
}

// armSetting writes the command that would move a setting into the composer,
// with its current value and legal values on the shortcut bar.
//
// It arms rather than sets, and that is the design rather than a shortcut.
// Every move then goes through /flags set, which is the one place that
// validates the value, refuses a startup flag, says which direction a safety
// flag moved, reloads what the change invalidated, and writes the whole thing
// into the transcript. A picker that flipped policy.file.mode with one keypress
// would be a second way to change a setting, and the second way is the one with
// no record.
func (a *App) armSetting(key string) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	spec, ok := a.settingSpec(key)
	if !ok {
		a.setNotice("no setting "+key, StatusWarn)
		return nil
	}
	if spec.Mutable == "startup" {
		// Named rather than armed. The registry is sealed once boot completes,
		// so the command this would compose is certain to be refused, and a
		// picker that spends the operator's keystroke on a guaranteed failure
		// is a picker they stop trusting.
		a.setNotice(key+" is startup-only and cannot be moved from here — it is "+
			spec.Value+" ("+spec.Origin+")", StatusWarn)
		return nil
	}
	if !v.Prompt.Empty() {
		a.setNotice("the composer has a draft, so it was not replaced; clear it with esc and choose "+
			key+" again", StatusWarn)
		return nil
	}
	v.Focus = CtxPrompt
	v.Prompt.SetValue("/flags set " + key + " ")

	hint := key + " is " + spec.Value + " (" + spec.Origin + ")"
	if len(spec.Choices) > 0 {
		hint += " — one of [" + strings.Join(spec.Choices, ", ") + "]"
	}
	status := StatusInfo
	if spec.Safety {
		hint += " — safety setting"
		status = StatusWarn
	}
	a.setNotice(hint, status)
	return nil
}

func (a *App) runCommand(name, args string) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	if a.Host != nil {
		known := false
		for _, spec := range a.Host.Commands() {
			if spec.Name == name {
				known = true
				break
			}
		}
		if !known {
			for _, spec := range uiCommands {
				if spec.Name == name {
					known = true
					break
				}
			}
		}
		if !known {
			// The aliases. They are not listed in either menu — one spelling
			// per command is enough to find it by — but they still resolve.
			switch name {
			case "exit", "cls":
				known = true
			}
		}
		if !known {
			a.setNotice("no command /"+name+" — ctrl+p to list", StatusWarn)
			return nil
		}
	}
	switch name {
	case "quit", "exit":
		return a.requestQuit()
	case "help":
		a.overlay = HelpOverlay()
		return nil
	case "clear", "cls":
		if v != nil {
			v.Scroll = NewScrollback()
		}
		return nil
	case "sessions":
		// The picker and its selection handler were both written; nothing ever
		// opened it. acceptOverlay has switched a.active on OverlaySessions
		// since the overlay was added, so this line is the whole of the missing
		// feature.
		a.overlay = SessionsOverlay(a.views, a.active)
		return nil
	case "theme":
		a.overlay = ThemeOverlay(a.Theme.Name)
		return nil
	case "settings":
		return a.openSettings()
	}

	// Everything above this line is answered by the UI and stays available
	// during a turn. Everything below is run by the harness, and the harness
	// runs one turn per session — a second one is dropped. The composer already
	// queues a prompt typed during a turn and says so; a slash command took a
	// different path, was dropped by the runner without a word, and looked from
	// here like the harness had hung.
	if v.Status.Busy {
		a.setNotice("/"+name+" needs this session idle — a turn is running; ctrl+c cancels it", StatusWarn)
		return nil
	}
	return []Effect{EffectCommand{SessionID: v.ID, Name: name, Args: args}}
}

func (a *App) openPalette() {
	var items []Item
	for _, spec := range a.menuCommands() {
		status := StatusNeutral
		if spec.Mutating {
			status = StatusWarn
		}
		detail := spec.Summary
		if spec.Args != "" {
			detail = spec.Args + "   " + detail
		}
		items = append(items, Item{
			Label: "/" + spec.Name, Detail: detail, Value: spec.Name, Status: status,
		})
	}
	a.overlay = NewOverlay(OverlayPalette, "commands", items, true)
}

// refreshCompletion opens, updates, or closes the inline dropdown to match what
// the prompt currently holds.
func (a *App) refreshCompletion() {
	v := a.Current()
	if v == nil || a.Host == nil {
		return
	}
	if a.overlay != nil && a.overlay.Kind != OverlayComplete {
		return
	}
	if v.Approval() != nil || v.Focus != CtxPrompt {
		return
	}
	trigger := v.Prompt.ActiveTrigger()
	if trigger.Kind != '/' {
		if a.overlay != nil && a.overlay.Kind == OverlayComplete {
			a.overlay = nil
		}
		return
	}
	var items []Item
	for _, spec := range a.menuCommands() {
		status := StatusNeutral
		if spec.Mutating {
			status = StatusWarn
		}
		// The usage hint, which CommandSpec.Args documents itself as being
		// "shown in the dropdown" and was shown only in the palette. A command
		// whose arguments are invisible where it is chosen is a command whose
		// arguments do not exist as far as the operator is concerned.
		detail := spec.Summary
		if spec.Args != "" {
			detail = spec.Args + "   " + detail
		}
		items = append(items, Item{
			Label: spec.Name, Detail: detail, Value: spec.Name, Status: status,
		})
	}
	o := NewOverlay(OverlayComplete, "commands", items, false)
	o.Trigger = trigger
	o.SetFilter(trigger.Query)
	if o.Empty() {
		a.overlay = nil
		return
	}
	a.overlay = o
}

func (a *App) scroll(t ActionScroll) []Effect {
	v := a.Current()
	if v == nil {
		return nil
	}
	if a.overlay != nil {
		a.overlay.Move(sign(t.Delta))
		return nil
	}
	if a.mode == ModeDashboard {
		a.dashboard.Move(sign(t.Delta), len(a.views))
		return nil
	}
	v.Scroll.ScrollBy(t.Delta)
	return nil
}

func sign(n int) int {
	if n < 0 {
		return -1
	}
	if n > 0 {
		return 1
	}
	return 0
}

// click routes a pointer press. The grammar is uniform across surfaces, so
// the mouse is learnable from any one of them: a click moves the highlight,
// and a click on the highlighted thing — a second click — confirms it. The
// keyboard's two-step (arrows then Enter) and the pointer's are the same
// shape, which is also what keeps a stray trackpad brush from answering
// anything.
func (a *App) click(t ActionClick) []Effect {
	// An open overlay owns the pointer entirely. Clicks were dropped on the
	// floor here once, which made the palette, the pickers, and the help list
	// keyboard-only surfaces.
	if a.overlay != nil {
		o := a.overlay
		if !a.overlayRect.Contains(t.X, t.Y) {
			// Click-away dismisses, as Esc does.
			if t.Button == 1 {
				a.overlay = nil
			}
			return nil
		}
		idx := o.ItemAt(t.X, t.Y)
		if idx < 0 {
			// The border, title, or query field: swallowed, not dismissed —
			// a click that misses a row by one cell must not close the list.
			return nil
		}
		if o.Sel() == idx {
			// Second click on the highlighted row accepts it.
			return a.accept()
		}
		o.MoveTo(idx)
		return nil
	}

	if a.mode == ModeDashboard {
		idx := a.dashboard.HitTest(render.Rect{X: 0, Y: 0, W: a.width, H: a.height}, t.Y, len(a.views))
		if idx < 0 {
			return nil
		}
		if idx == a.dashboard.Selected() && t.Button == 1 && a.doubleClick(t.X, t.Y) {
			// Second click on the highlighted row opens the session.
			a.active = idx
			a.mode = ModeSession
			return nil
		}
		a.doubleClick(t.X, t.Y)
		a.dashboard.Select(idx)
		return nil
	}

	v := a.Current()
	if v == nil {
		return nil
	}

	// The session strip is app chrome, above the session's own layout.
	if t.Button == 1 && len(a.tabs) > 0 {
		for _, tab := range a.tabs {
			if !tab.Rect.Contains(t.X, t.Y) {
				continue
			}
			if tab.IsNewButton {
				return []Effect{EffectNewSession{}}
			}
			if tab.SessionIndex != a.active {
				a.active = tab.SessionIndex
			}
			return nil
		}
		if a.tabRow.Contains(t.X, t.Y) {
			return nil
		}
	}

	// An approval card is modal for the pointer exactly as it is for the
	// keyboard: until it is answered, clicks belong to it and to nothing
	// behind it.
	if card := v.Approval(); card != nil {
		accept, _ := card.Click(t.X, t.Y)
		if !accept {
			return nil
		}
		return a.acceptApproval()
	}

	// The composer: click focuses it and puts the caret where the click
	// landed, the courtesy every text field extends.
	if t.Button == 1 && v.promptRect.Contains(t.X, t.Y) {
		v.Focus = CtxPrompt
		v.Scroll.SelectNone()
		v.Prompt.SetCursor(v.Prompt.IndexAt(v.promptRect.W,
			t.Y-v.promptRect.Y, t.X-v.promptRect.X))
		return nil
	}

	// The scrollbar column is a control, not content: a press on it grabs the
	// thumb and the drag scrolls, rather than selecting transcript text. It
	// sits beside the content rect, so it is tested before the content is.
	if bar := v.Scroll.ScrollbarCol(); t.Button == 1 && bar >= 0 && t.X == bar &&
		t.Y >= v.Scroll.view.Y && t.Y < v.Scroll.view.Bottom() {
		a.dragBar = true
		v.Scroll.ScrollToFraction(scrollFraction(t.Y, v.Scroll.view))
		return nil
	}

	// Hit-tested against the rect the last frame actually drew, rather than
	// against one recomputed here. Recomputing means two versions of the layout
	// arithmetic, and the one in a click handler is the one that goes stale.
	if v.Scroll.Contains(t.X, t.Y) {
		v.Focus = CtxScrollback
		if t.Button == 3 {
			// Right-click copies the entry under the pointer, matching y.
			if v.Scroll.SelectAt(t.Y) {
				if text := v.Scroll.SelectedText(); text != "" {
					a.setNotice("copied "+itoa(len(text))+" bytes", StatusInfo)
					return []Effect{EffectCopy{Text: text}}
				}
			}
			return nil
		}
		if v.Scroll.SelectAt(t.Y) && a.doubleClick(t.X, t.Y) {
			v.Scroll.ToggleFold()
		}
		return nil
	}

	v.Focus = CtxPrompt
	v.Scroll.SelectNone()
	return nil
}

// motion handles the pointer moving: a scrollbar drag in progress, or a hover
// crossing an open overlay's rows.
func (a *App) motion(t ActionMotion) []Effect {
	if a.dragBar {
		if v := a.Current(); v != nil && v.Scroll.ScrollbarCol() >= 0 {
			v.Scroll.ScrollToFraction(scrollFraction(t.Y, v.Scroll.view))
		}
		return nil
	}
	if t.Button == 0 && a.overlay != nil {
		a.overlay.HoverAt(t.X, t.Y)
	}
	return nil
}

// scrollFraction maps a row inside the transcript viewport to a fraction of
// the scroll travel, for a dragged thumb.
func scrollFraction(y int, view render.Rect) float64 {
	if view.H <= 1 {
		return 0
	}
	return float64(y-view.Y) / float64(view.H-1)
}

// doubleClick reports whether this press is the second at the same cell
// within the confirmation window, and records it either way.
func (a *App) doubleClick(x, y int) bool {
	now := time.Now()
	again := x == a.lastClickX && y == a.lastClickY && now.Sub(a.lastClick) < DoublePress
	a.lastClick, a.lastClickX, a.lastClickY = now, x, y
	return again
}

func (a *App) setNotice(text string, kind StatusKind) {
	a.notice, a.noticeAt, a.noticeKind = text, time.Now(), kind
}

// Notice is the transient message, for the runner's shortcut bar.
func (a *App) Notice() (string, StatusKind) { return a.notice, a.noticeKind }

// Draw paints the whole frame and returns the caret position.
func (a *App) Draw(b *render.Buffer) render.Rect {
	w, h := b.W, b.H
	a.width, a.height = w, h
	full := render.Rect{X: 0, Y: 0, W: w, H: h}
	b.Fill(full, ' ', a.Theme.Base())
	a.overlayRect = render.Rect{}

	var caret render.Rect
	switch {
	case a.mode == ModeDashboard:
		a.tabs = nil
		a.tabRow = render.Rect{}
		active := ""
		if v := a.Current(); v != nil {
			active = v.ID
		}
		a.dashboard.Draw(b, full, a.Theme, a.views, active, a.tick, a.fxOn())
	case a.Current() != nil:
		area := full
		if len(a.views) > 1 {
			// The session strip takes a row of its own only when there is
			// more than one session to switch between — a single session
			// keeps its transcript row rather than losing it to a strip that
			// says nothing.
			a.tabRow, area = area.SplitTop(1)
			a.tabs = DrawSessionTabBar(b, a.tabRow, a.Theme, a.views, a.active, a.tick)
		} else {
			a.tabs = nil
			a.tabRow = render.Rect{}
		}
		caret = a.Current().Draw(b, area, a.Theme, a.tick, a.series.Values(), a.fxOn())
	default:
		a.tabs = nil
		a.tabRow = render.Rect{}
		a.drawEmpty(b, full)
	}

	if a.notice != "" {
		a.drawNotice(b, full)
	}
	if a.overlay != nil {
		if c := a.drawOverlay(b, full); c.W > 0 {
			caret = c
		} else {
			caret = render.Rect{}
		}
	}
	return caret
}

func (a *App) drawEmpty(b *render.Buffer, r render.Rect) {
	th := a.Theme
	blocks := th.LogoBlocks()

	// The idle backdrop: a rain field, drawn first so the mark and the hint
	// paint over it. Procedural per the fx package's rules — a pure function
	// of the tick — and absent entirely when animation is off or the terminal
	// cannot carry the colour.
	if a.fxOn() {
		fx.Rain{Gap: 3, Tail: 9}.Draw(b, r, a.tick, th.AccentDim, th.Bg, th.Unicode)
	}

	// The mark is centred on the pane rather than pinned to a row, and it
	// shrinks through its own rungs, so a narrow split pane gets a smaller mark
	// instead of a corrupted frame.
	size := logo.Fit(r.W, r.H, blocks)
	top := r.Y + (r.H-size.Height())/2 - 1
	if top < r.Y {
		top = r.Y
	}
	used := logo.Draw(b, render.Rect{X: r.X, Y: top, W: r.W, H: r.H - (top - r.Y)},
		th.Logo(), blocks, "the DevCouncil execution harness")

	// The hint types itself out once at startup — a first-run cue that the
	// keys on it are live, timed to finish inside the first couple of seconds.
	hintText := "ctrl+n  new session      ctrl+p  commands      ctrl+q  quit"
	if a.fxOn() {
		hintText = fx.RevealText(hintText, a.tick, 0, 4)
	}
	hint := render.Styled(hintText, th.Subtle())
	if y := top + used + 1; y < r.Bottom() && hintText != "" {
		hint.Truncate(r.W).Draw(b, r.X+(r.W-hint.Width())/2, y)
	}
}

func (a *App) drawNotice(b *render.Buffer, r render.Rect) {
	th := a.Theme
	style := th.Status(a.noticeKind)
	style.Bg = th.BgRaised
	text := " " + a.notice + " "
	w := render.StringWidth(text)
	if w > r.W-4 {
		w = r.W - 4
	}
	x := r.Right() - w - 2

	// Below the status bar, and below the strict banner when there is one. A
	// transient message may cover transcript, which scrolls back; it may not
	// cover the statement that this run's results are not strict, which is the
	// one thing on screen with nowhere else to be read.
	y := r.Y + 1
	if v := a.Current(); v != nil {
		if strict, _ := v.Status.StrictSummary(); !strict {
			y = r.Y + 2
		}
	}
	if y >= r.Bottom() {
		y = r.Bottom() - 1
	}
	if x < r.X {
		x = r.X
	}
	b.Fill(render.Rect{X: x, Y: y, W: w, H: 1}, ' ', style)
	render.Styled(text, style).Truncate(w).Draw(b, x, y)
}

func (a *App) drawOverlay(b *render.Buffer, r render.Rect) render.Rect {
	o := a.overlay
	// The rect is recorded as well as drawn, so the click handler tests
	// against the frame the operator is looking at rather than recomputing it.
	switch o.Kind {
	case OverlayComplete:
		// Anchored just above the composer, where the text being completed is.
		h := o.Height(10)
		w := r.W / 2
		if w < 40 {
			w = r.W - 4
		}
		v := a.Current()
		promptH := 3
		if v != nil {
			promptH = v.promptHeight(r.W) + 2
		}
		y := r.Bottom() - 1 - promptH - h
		if y < r.Y {
			y = r.Y
		}
		a.overlayRect = render.Rect{X: r.X + 2, Y: y, W: w, H: h}
	default:
		h := o.Height(r.H - 6)
		w := r.W * 2 / 3
		if w < 50 {
			w = r.W - 4
		}
		if w > r.W-4 {
			w = r.W - 4
		}
		x := r.X + (r.W-w)/2
		y := r.Y + (r.H-h)/3
		a.overlayRect = render.Rect{X: x, Y: y, W: w, H: h}
	}
	return o.Draw(b, a.overlayRect, a.Theme)
}
