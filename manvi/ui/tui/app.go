package tui

import (
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/fx"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
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
	a.dashboard.Invalidate()
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
		a.dashboard.Invalidate()
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

func (a *App) setNotice(text string, kind StatusKind) {
	a.notice, a.noticeAt, a.noticeKind = text, time.Now(), kind
}

// Notice is the transient message, for the runner's shortcut bar.
func (a *App) Notice() (string, StatusKind) { return a.notice, a.noticeKind }
