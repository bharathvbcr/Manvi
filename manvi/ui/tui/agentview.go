package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"manvi/ui"
	"manvi/ui/fx"
	"manvi/ui/render"
)

// AgentView is one session: a transcript, a composer, the approvals waiting on
// an operator, and the status that describes the run.
type AgentView struct {
	ID    string
	Title string

	Scroll *Scrollback
	Prompt *Prompt
	Status Status

	// Focus is CtxPrompt or CtxScrollback.
	Focus Context

	// approval is the card currently blocking, and pending is the queue behind
	// it. Approvals are queued rather than stacked: two cards on screen at once
	// invites answering the wrong one, and each blocked call is waiting on its
	// own reply channel anyway.
	approval *ApprovalCard
	pending  []*ApprovalCard

	// queued holds follow-up prompts typed while a turn was running.
	queued []string

	// promptRect is the composer's editable area as last drawn, so a click
	// lands on the caret position the operator aimed at rather than on a
	// recomputed guess at the layout.
	promptRect render.Rect

	// lastEscape supports the double-Escape binding.
	lastEscape time.Time
	// firstActivity and lastActivity drive the dashboard's timing column.
	firstActivity time.Time
	lastActivity  time.Time
	// err is the last turn error, shown until the next turn starts.
	err string
}

// NewAgentView builds an empty session view.
func NewAgentView(id, title string) *AgentView {
	return &AgentView{
		ID:     id,
		Title:  title,
		Scroll: NewScrollback(),
		Prompt: NewPrompt(),
		Focus:  CtxPrompt,
	}
}

// Apply folds a harness event into the view.
func (v *AgentView) Apply(ev ui.Event) {
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
	// and on the posture chip; it does not erase what was produced before it.
	for _, w := range ev.Weakened {
		if !slices.Contains(v.Status.Weakened, w) {
			v.Status.Weakened = append(v.Status.Weakened, w)
		}
	}
}

// SetBusy marks the session running or idle.
func (v *AgentView) SetBusy(busy bool, label string) {
	v.Status.Busy = busy
	v.Status.BusyLabel = label
	if busy {
		v.err = ""
	}
}

// Approval returns the card blocking this session, if any.
func (v *AgentView) Approval() *ApprovalCard { return v.approval }

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
func (v *AgentView) PendingApprovals() int { return len(v.pending) }

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
func (v *AgentView) Queued() []string { return v.queued }

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
	inner := width - 4
	if inner < 8 {
		inner = 8
	}
	return v.Prompt.Height(inner, max)
}

// SetTitle updates the session title.
func (v *AgentView) SetTitle(title string) {
	v.Title = title
}

// Draw paints the whole session view and returns the caret position.
//
// fxOn gates the ambient motion: the composer border's sweep while a turn
// runs, the status bar's breathing busy label and throughput sparkline, and
// the approval card's pulsing frame. It is one flag for all of them because
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

	area := r
	statusRow, area := area.SplitTop(1)
	DrawStatusBar(b, statusRow, th, v.Status, tick, spark, fxOn)

	if strict, _ := v.Status.StrictSummary(); !strict {
		var banner render.Rect
		banner, area = area.SplitTop(1)
		DrawStrictBanner(b, banner, th, v.Status)
	}

	area, hintRow := area.SplitBottom(1)

	// The composer, sized to its content, from the bottom up.
	promptH := v.promptHeight(area.W) + 2 // frame
	if promptH > area.H/2 {
		promptH = area.H / 2
	}
	if promptH < 3 {
		promptH = 3
	}
	area, promptRect := area.SplitBottom(promptH)

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
	}

	v.Scroll.Draw(b, area, th, v.Focus == CtxScrollback)

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
	DrawShortcutBar(b, hintRow, th, ctx, extra...)
	return caret
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
	focused := v.Focus == CtxPrompt && v.approval == nil
	border := th.Border
	if focused {
		border = th.BorderFocus
	}

	title := render.Styled(" compose ", render.Style{Fg: th.FgSubtle, Bg: th.Bg})
	if v.Status.Busy {
		title = render.Styled(" queue follow-up ", render.Style{Fg: th.Warning, Bg: th.Bg})
	}
	if v.Prompt.Multiline {
		title = title.Append("multiline ", render.Style{Fg: th.Accent, Bg: th.Bg})
	}

	var subtitle render.Line
	if v.err != "" {
		subtitle = render.Styled(" "+render.TruncateWidth(v.err, r.W-8, "…")+" ",
			render.Style{Fg: th.Danger, Bg: th.Bg})
	} else if !v.Prompt.Empty() {
		chars := len(v.Prompt.Value())
		tokens := chars/4 + 1
		subtitle = render.Styled(fmt.Sprintf(" %d chars • ~%d tokens ", chars, tokens),
			render.Style{Fg: th.FgSubtle, Bg: th.Bg})
	} else if r.W > 50 {
		subtitle = render.Styled(" / commands • @ context • Tab focus ",
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

	// While a turn runs, a beam sweeps the top edge: the transcript is
	// streaming or the tool is out, and the frame should say so even when the
	// visible rows are momentarily still. The cold cells repaint in the
	// border's own style, so the sweep leaves nothing behind it.
	if fxOn && v.Status.Busy && r.W > 4 {
		edge := render.Rect{X: r.X + 1, Y: r.Y, W: r.W - 2, H: 1}
		fx.SweepStyled(b, edge, tick, borderFor(th).Top, th.Accent,
			render.Style{Fg: border, Bg: th.Bg})
	}

	inner = inner.Pad(0, 1, 0, 1)
	v.promptRect = inner
	return v.Prompt.Draw(b, inner, th, focused)
}
