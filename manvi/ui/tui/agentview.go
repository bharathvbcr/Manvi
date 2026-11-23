package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// AgentView is one session: a transcript, a composer, the approvals waiting on
// an operator, and the status that describes the run.
type AgentView struct {
	ID    string
	Title string

	Scroll    *Scrollback
	Prompt    *Prompt
	Inspector *Inspector
	Status    Status

	// Focus is CtxPrompt, CtxScrollback, or CtxInspector.
	Focus Context

	// approval is the card currently blocking, and pending is the queue behind
	// it. Approvals are queued rather than stacked: two cards on screen at once
	// invites answering the wrong one, and each blocked call is waiting on its
	// own reply channel anyway.
	approval *ApprovalCard
	pending  []*ApprovalCard

	// queued holds follow-up prompts typed while a turn was running.
	queued []string

	// phase is what the running turn was last seen doing, and outcome how the
	// last turn ended, until the operator has seen it. Both are for the
	// terminal's tab (termstatus.go), which is read by someone looking
	// elsewhere.
	phase   string
	outcome turnOutcome

	// promptRect is the composer's editable area as last drawn, so a click
	// lands on the caret position the operator aimed at rather than on a
	// recomputed guess at the layout.
	promptRect render.Rect
	// latestPill is the clickable "↓ latest" jump pill shown when scrolled up.
	latestPill render.Rect

	// lastEscape supports the double-Escape binding.
	lastEscape time.Time
	// firstActivity and lastActivity drive the dashboard's timing column.
	firstActivity time.Time
	lastActivity  time.Time
	// busySince is when the running turn started, for the working row's clock.
	busySince time.Time
	// err is the last turn error, shown until the next turn starts.
	err string
}

// NewAgentView builds an empty session view.
func NewAgentView(id, title string) *AgentView {
	return &AgentView{
		ID:        id,
		Title:     title,
		Scroll:    NewScrollback(),
		Prompt:    NewPrompt(),
		Inspector: NewInspector(),
		Focus:     CtxPrompt,
	}
}

// Apply folds a harness event into the view.
func (v *AgentView) Apply(ev ui.Event) {
	// Wire-only kinds touch nothing here: not the transcript, not the status
	// accumulation below, which would otherwise fold a verify.report's Degraded
	// into the "unrun" chip and flip the session non-strict for a check the
	// terminal never draws. Nor lastActivity: they trail visible activity, so
	// they add nothing the dashboard's timing column does not already have.
	if ev.Kind.WireOnly() {
		return
	}
	v.Scroll.Append(ev)
	v.lastActivity = time.Now()
	if v.firstActivity.IsZero() {
		v.firstActivity = v.lastActivity
	}

	// Posture is the one status field that answers "right now" rather than
	// "ever", so it is taken from whatever event carries it rather than from
	// the session banner alone. It became a live value the moment harness.posture
	// could be moved from the composer, and a chip still reading yolo after the
	// operator had gone back to strict would be describing a run that no longer
	// exists. The accumulating fields below keep the history honest: a posture
	// that was ever relaxed stays in Weakened, so StrictSummary does not stop
	// disqualifying the session just because the chip changed.
	if ev.Posture != "" {
		v.Status.Posture = ev.Posture
	}

	switch ev.Kind {
	case ui.KindSessionStart:
		if ev.Model != "" {
			v.Status.Model = ev.Model
		}
	case ui.KindUsage:
		v.Status.InputTokens += ev.InputTokens
		v.Status.OutputTokens += ev.OutputTokens
	case ui.KindLease:
		if ev.TaskID != "" {
			v.Status.TaskID = ev.TaskID
		}
	case ui.KindError:
		v.err = ev.Text
	case ui.KindReasoning:
		v.phase = phaseThinking
	case ui.KindText:
		v.phase = phaseWriting
	case ui.KindToolStart:
		// The tool's name and nothing else. Its arguments are where a path or
		// a command line would be, and this ends up on a tab.
		v.phase = phaseWorking
		if ev.Tool != "" {
			v.phase = "running " + ev.Tool
		}
	case ui.KindToolResult:
		v.phase = phaseWorking
	}

	// Qualifications accumulate on the status regardless of which event carried
	// them. A grant applied during a tool call and one applied during a policy
	// check are the same fact about the run.
	if ev.GrantID != "" {
		v.Status.Grants++
	}
	for _, d := range ev.Degraded {
		if !slices.Contains(v.Status.Degraded, d) {
			v.Status.Degraded = append(v.Status.Degraded, d)
		}
	}
	// Accumulated and never retracted, deliberately. StrictSummary answers
	// "can this session's results be described as strict", and a turn that ran
	// while a safety flag was off does not become strict retroactively because
	// the flag was put back. Tightening a setting is visible in the transcript
	// and on the posture; it does not erase what was produced before it.
	for _, w := range ev.Weakened {
		if !slices.Contains(v.Status.Weakened, w) {
			v.Status.Weakened = append(v.Status.Weakened, w)
		}
	}
}

// SetBusy marks the session running or idle.
func (v *AgentView) SetBusy(busy bool, label string) {
	if v == nil {
		return
	}
	if busy && !v.Status.Busy {
		v.busySince = time.Now()
	}
	v.Status.Busy = busy
	v.Status.BusyLabel = label
	v.phase = phaseWorking
	if busy {
		// A turn starting is activity. Without this a stall is measured from
		// the last event of the turn before, and a turn begun on a session that
		// had sat idle for a minute would open already "stalled", on the status
		// bar and on the tab, until its first event arrived.
		v.lastActivity = time.Now()
		v.err = ""
	}
}

// activity is what the running turn is doing, read off the transcript's newest
// entry: the model is thinking, or writing, or a tool is out. The host says
// only that a turn is running; a label it set to something else is its own and
// is shown as it is.
func (v *AgentView) activity() string {
	if l := v.Status.BusyLabel; l != "" && l != "working" {
		return l
	}
	entries := v.Scroll.Entries()
	if len(entries) == 0 {
		return "Working"
	}
	switch last := entries[len(entries)-1]; last.Event.Kind {
	case ui.KindReasoning:
		return "Thinking"
	case ui.KindText:
		return "Writing"
	case ui.KindToolStart:
		if last.Event.Tool != "" {
			return doingPhrase(last.Event)
		}
	}
	return "Working"
}

// Approval returns the card blocking this session, if any.
func (v *AgentView) Approval() *ApprovalCard {
	if v == nil {
		return nil
	}
	return v.approval
}

// PushApproval queues a request, activating it if nothing is blocking.
func (v *AgentView) PushApproval(c *ApprovalCard) {
	if v.approval == nil {
		v.approval = c
		return
	}
	v.pending = append(v.pending, c)
}

// PopApproval retires the active card and promotes the next.
func (v *AgentView) PopApproval() {
	v.approval = nil
	if len(v.pending) > 0 {
		v.approval, v.pending = v.pending[0], v.pending[1:]
	}
}

// PendingApprovals is how many are still waiting behind the active one.
func (v *AgentView) PendingApprovals() int {
	if v == nil {
		return 0
	}
	return len(v.pending)
}

// Queue adds a follow-up to run when the current turn ends.
func (v *AgentView) Queue(text string) { v.queued = append(v.queued, text) }

// Dequeue takes the next queued follow-up.
func (v *AgentView) Dequeue() (string, bool) {
	if len(v.queued) == 0 {
		return "", false
	}
	next := v.queued[0]
	v.queued = v.queued[1:]
	return next, true
}

// Queued exposes the pending follow-ups.
func (v *AgentView) Queued() []string {
	if v == nil {
		return nil
	}
	return v.queued
}

// EscapePressed implements the double-Escape rule and reports whether this
// press was the second of a pair.
func (v *AgentView) EscapePressed(window time.Duration) bool {
	now := time.Now()
	double := !v.lastEscape.IsZero() && now.Sub(v.lastEscape) < window
	v.lastEscape = now
	if double {
		v.lastEscape = time.Time{}
	}
	return double
}

// Activity reports when the session last did anything, for the dashboard.
func (v *AgentView) Activity() time.Time { return v.lastActivity }

// Error is the last turn failure, or empty.
func (v *AgentView) Error() string { return v.err }

// promptHeight is how tall the composer box should be.
func (v *AgentView) promptHeight(width int) int {
	const max = 10
	// The frame, a column of padding each side, and the prompt mark.
	inner := width - 4 - promptMark
	if inner < 8 {
		inner = 8
	}
	return v.Prompt.Height(inner, max)
}

// SetTitle updates the session title.
func (v *AgentView) SetTitle(title string) {
	if v == nil {
		return
	}
	v.Title = title
}

// Draw paints the whole session view and returns the caret position.
//
// fxOn gates the ambient motion: the working row's breathing label, the status
// bar's throughput sparkline, and the approval card's pulsing frame. It is one flag for all of them because
// they are one courtesy — an operator who turned animation off turned it off.
func (v *AgentView) Draw(b *render.Buffer, r render.Rect, th Theme, tick int, spark []float64, fxOn bool) render.Rect {
	if r.Empty() {
		v.promptRect = render.Rect{}
		return render.Rect{}
	}
	b.Fill(r, ' ', th.Base())

	// Rotate placeholder tip if empty
	if v.Prompt.Empty() {
		v.Prompt.Placeholder = Tip(tick / 50)
	}

	// Calculate stall duration during busy turns
	if v.Status.Busy && !v.lastActivity.IsZero() {
		v.Status.StallSecs = int(time.Since(v.lastActivity).Seconds())
	} else {
		v.Status.StallSecs = 0
	}
	v.Status.BusySecs = 0
	if v.Status.Busy && !v.busySince.IsZero() {
		v.Status.BusySecs = int(time.Since(v.busySince).Seconds())
	}

	// Nothing is pinned to the top: the transcript has the screen down to the
	// composer, and what the session is — posture, model, branch, context — is
	// one quiet row under it, shared with the keys of the moment.
	area := r
	area, footer := area.SplitBottom(1)

	// Adaptive Workbench split: when the Inspector Drawer is open and the
	// terminal is wide enough (>= inspectorMinWidth), split 62% main / 38%
	// drawer. When closed on wide screens in stacked mode, center the reading
	// column to a comfortable ~104-column measure.
	var drawerRect render.Rect
	if v.Inspector != nil && v.Inspector.Open && area.W >= inspectorMinWidth {
		dw := area.W * 38 / 100
		if dw < 40 {
			dw = 40
		}
		if dw > 68 {
			dw = 68
		}
		area, drawerRect = area.SplitRight(dw)
	} else if v.Scroll.StackWork && area.W > 116 {
		pad := (area.W - 104) / 2
		area = area.Pad(0, pad, 0, pad)
	}
	if drawerRect.Empty() && v.Focus == CtxInspector {
		v.Focus = CtxPrompt
	}

	// A run that is not strict says so on a row of its own, directly above
	// where the operator types: it is the one statement on screen that changes
	// what every result above it means, and it is never folded into the row of
	// status under the composer, which a narrow terminal abbreviates. Its row
	// is set aside before the composer is sized, so on a terminal too short
	// for both it is the composer that gives way.
	strict, _ := v.Status.StrictSummary()
	room := area.H
	if !strict {
		room--
	}

	// The composer, sized to its content, from the bottom up.
	promptH := v.promptHeight(area.W) + 2 // frame
	if promptH > room/2 {
		promptH = room / 2
	}
	if promptH < 3 {
		promptH = 3
	}
	if promptH > room {
		promptH = max(room, 0)
	}
	area, promptRect := area.SplitBottom(promptH)

	if !strict {
		var banner render.Rect
		area, banner = area.SplitBottom(1)
		DrawStrictBanner(b, banner, th, v.Status)
	}

	// Queued follow-ups sit directly above the composer, where they explain why
	// typing while busy did not send.
	if len(v.queued) > 0 && area.H > 4 {
		var queue render.Rect
		h := len(v.queued)
		if h > 3 {
			h = 3
		}
		area, queue = area.SplitBottom(h + 1)
		v.drawQueue(b, queue, th)
	}

	// The working row, between the transcript and whatever is typed under it.
	// An approval card takes its place: the turn is then waiting on the
	// operator, and the card says so itself.
	if v.Status.Busy && v.approval == nil && area.H > 4 {
		var working render.Rect
		area, working = area.SplitBottom(1)
		st := v.Status
		st.BusyLabel = v.activity()
		DrawActivity(b, working, th, st, tick, fxOn)
		// When the Inspector Drawer is closed and stacked mode is active,
		// regenerate the active thinking trace in one compact line directly
		// above the working row so it never floods the transcript.
		if (v.Inspector == nil || !v.Inspector.Open) && v.Scroll.StackWork && area.H > 6 {
			if thought := v.liveThoughtPreview(); thought != "" {
				var thinkRow render.Rect
				area, thinkRow = area.SplitBottom(1)
				b.Fill(thinkRow, ' ', th.Base())
				render.Styled("  "+th.Glyphs().Thought+" ", th.Subtle()).
					Append(thought, th.Subtle().With(render.Italic)).
					Truncate(thinkRow.W).Draw(b, thinkRow.X, thinkRow.Y)
			}
		}
		if area.H > 8 {
			// A blank row above it, where there is the height: the row is the
			// state of the turn, not the last line of what the turn printed.
			var spacer render.Rect
			area, spacer = area.SplitBottom(1)
			b.Fill(spacer, ' ', th.Base())
		}
	}

	// A blocking approval takes the space above the composer. It is drawn last
	// in the stack so nothing can overlap it.
	var caret render.Rect
	if v.approval != nil && area.H > 6 {
		v.approval.Index = 1
		v.approval.Total = 1 + v.PendingApprovals()
		h := v.approval.Height(th, area.W)
		if h > area.H-2 {
			h = area.H - 2
		}
		var card render.Rect
		area, card = area.SplitBottom(h)
		caret = v.approval.Draw(b, card, th, tick, fxOn)
	} else if v.approval != nil {
		// Too small to draw: it must also be unclickable, or a control the
		// operator cannot see would still be answering clicks.
		v.approval.cardRect = render.Rect{}
		v.approval.leave()
	}

	v.Scroll.Draw(b, area, th, v.Focus == CtxScrollback)

	// Floating clickable "↓ latest" pill when the user has scrolled up.
	v.latestPill = render.Rect{}
	if !v.Scroll.Following() && area.H >= 4 && area.W >= 24 {
		lbl := " ↓ latest "
		if !th.Unicode {
			lbl = " v latest "
		}
		pw := render.StringWidth(lbl)
		px := area.Right() - pw - 2
		py := area.Bottom() - 1
		if px >= area.X && py >= area.Y {
			pillStyle := render.Style{Fg: th.Fg, Bg: th.BgRaised, Attrs: render.Bold}
			if th.monochrome() {
				pillStyle = render.Style{Attrs: render.Reverse}
			}
			v.latestPill = render.Rect{X: px, Y: py, W: pw, H: 1}
			render.Styled(lbl, pillStyle).Draw(b, px, py)
		}
	}

	if v.Inspector != nil {
		v.Inspector.Draw(b, drawerRect, th, v, v.Focus == CtxInspector)
	}

	promptCaret := v.drawPrompt(b, promptRect, th, tick, fxOn)
	if v.approval == nil {
		caret = promptCaret
	}

	ctx := v.Focus
	if v.approval != nil {
		ctx = CtxApproval
	}
	var extra []string
	if n := v.PendingApprovals(); n > 0 {
		extra = append(extra, itoa(n)+" more approval(s) waiting")
	}
	if !v.Scroll.Following() {
		extra = append(extra, "scrolled back — G for latest")
	}
	if v.Scroll.SearchQuery() != "" {
		extra = append(extra, v.Scroll.SearchStatus()+" (n/N)")
	}
	DrawFooter(b, footer, th, v.Status, ctx, tick, spark, fxOn, extra...)
	return caret
}

// liveThoughtPreview returns the newest non-empty line of a live reasoning
// entry for the compact single-slot thinking strip above the working row.
func (v *AgentView) liveThoughtPreview() string {
	entries := v.Scroll.Entries()
	if len(entries) == 0 {
		return ""
	}
	last := entries[len(entries)-1]
	if last.Event.Kind != ui.KindReasoning || !last.live {
		return ""
	}
	text := strings.TrimSpace(last.text())
	if text == "" {
		return ""
	}
	if i := strings.LastIndexByte(text, '\n'); i >= 0 {
		text = strings.TrimSpace(text[i+1:])
	}
	return ui.OneLine(text)
}

func (v *AgentView) drawQueue(b *render.Buffer, r render.Rect, th Theme) {
	if r.Empty() {
		return
	}
	b.Fill(r, ' ', th.Base())
	g := th.Glyphs()
	render.Styled("queued while busy — sent when this turn ends", th.Subtle()).
		DrawIn(b, render.Rect{X: r.X + 1, Y: r.Y, W: r.W - 2, H: 1})
	for i, q := range v.queued {
		if i+1 >= r.H {
			break
		}
		render.Styled("  "+g.Arrow+" ", th.Status(StatusInfo)).
			Append(strings.ReplaceAll(q, "\n", " "), th.Muted()).
			Truncate(r.W-2).Draw(b, r.X+1, r.Y+1+i)
	}
}

func (v *AgentView) drawPrompt(b *render.Buffer, r render.Rect, th Theme, tick int, fxOn bool) render.Rect {
	// The frame is a neutral rule, a step brighter while the composer has the
	// keys. In the brand colour it was the largest area of red on the screen,
	// in a red a glance could not tell from a failure's; the prompt mark inside
	// it carries the brand and the focus both.
	focused := v.Focus == CtxPrompt && v.approval == nil
	border := th.Border
	if focused {
		border = th.FgSubtle
	}

	// The frame carries a word only when the word changes what Enter does: a
	// turn is running, so a send is queued, or the composer is in multiline.
	// Idle, it is an empty frame around the prompt. The label "compose" told
	// nobody anything, and the hints that sat in the bottom edge are the
	// shortcut bar's, one row down.
	var title render.Line
	if v.Status.Busy {
		title = render.Styled(" queue follow-up ", render.Style{Fg: th.FgMuted, Bg: th.Bg})
	}
	if v.Prompt.Multiline {
		if len(title) == 0 {
			title = render.Styled(" ", th.Base())
		}
		title = title.Append("multiline ", render.Style{Fg: th.Accent, Bg: th.Bg})
	}

	var subtitle render.Line
	if v.err != "" {
		subtitle = render.Styled(" "+render.TruncateWidth(v.err, r.W-8, "…")+" ",
			render.Style{Fg: th.Danger, Bg: th.Bg})
	} else if chars := len(v.Prompt.Value()); chars >= draftCountFrom {
		// A count for a draft long enough to wonder about, not for every word.
		subtitle = render.Styled(fmt.Sprintf(" %d chars %s ~%d tokens ", chars, th.Glyphs().Dot, chars/4+1),
			render.Style{Fg: th.FgSubtle, Bg: th.Bg})
	}

	fill := th.Base()
	inner := render.Box{
		Border:   borderFor(th),
		Style:    render.Style{Fg: border, Bg: th.Bg},
		Title:    title,
		Subtitle: subtitle,
		Fill:     &fill,
	}.Draw(b, r)

	inner = inner.Pad(0, 1, 0, 1)
	if inner.W > promptMark {
		// The same mark the transcript puts on what was asked, so the composer
		// reads as the next of those.
		mark := render.Style{Fg: th.FgSubtle, Bg: th.Bg}
		if focused {
			mark = th.AccentStyle()
		}
		b.SetString(inner.X, inner.Y, th.Glyphs().Prompt, mark)
		inner.X += promptMark
		inner.W -= promptMark
	}
	v.promptRect = inner
	return v.Prompt.Draw(b, inner, th, focused)
}

const (
	// promptMark is the columns the composer's prompt mark and its space take.
	promptMark = 2
	// draftCountFrom is the draft length at which the composer starts counting.
	draftCountFrom = 200
)
