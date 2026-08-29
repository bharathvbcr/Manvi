package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"manvi/ui"
	"manvi/ui/fx"
	"manvi/ui/render"
)

// padLabel renders a field label at this card's fixed label column, so the rows
// stay aligned when the subject row is "command" rather than "path".
func padLabel(label string) string {
	const width = 8
	for len(label) < width {
		label += " "
	}
	return label
}

// ApprovalCard is the blocking permission prompt.
//
// It is modal, and it is the one place in this UI where that is not a matter of
// taste. This is the human-in-the-loop control: if the keys behind it stayed
// live, a queued keystroke could answer it, and an approval answered by
// accident is indistinguishable in the ledger from one answered on purpose.
//
// The card also refuses to offer an allow it cannot honour. A hard rule is
// shown with a single acknowledge option, because no authority clears it — an
// "allow" that is going to be refused downstream teaches an operator that the
// control is advisory.
type ApprovalCard struct {
	Request ui.Request
	Reply   chan ui.Decision

	// option indexes the choices.
	option int
	// stage is which half of the card is active.
	stage approvalStage
	// reason is the justification, required for an allow.
	reason *Prompt
	// arrived is when the request was raised, so the card can show how long a
	// turn has been blocked on a human.
	arrived time.Time
	// picked holds the ticked rows of a multi-select question. Nothing else
	// uses it: an allow/deny card and a single-select question both answer from
	// option alone.
	picked map[int]bool

	// Index and Total track position in the pending approval queue.
	Index int
	Total int

	// cardRect is the whole card as last drawn, and optionRows and reasonRect
	// are its controls, recorded against screen coordinates so a click is
	// tested against what the operator is looking at rather than a layout
	// recomputed here.
	cardRect   render.Rect
	optionRows []render.Rect
	reasonRect render.Rect
}

// writeInOption is the last row of a question.
//
// It is appended to the model's own options rather than being a separate
// control because the tool this card serves advertises a free-text answer. A
// card that offered only the listed options would quietly narrow every question
// to the choices the model happened to think of, and the operator would have no
// way to say the thing none of them says.
const writeInOption = "write in a different answer"

type approvalStage int

const (
	stageChoose approvalStage = iota
	stageReason
)

// NewApprovalCard builds the card for a request.
func NewApprovalCard(req ui.Request, reply chan ui.Decision) *ApprovalCard {
	p := NewPrompt()
	p.Placeholder = "why this is safe — recorded on the grant"
	return &ApprovalCard{Request: req, Reply: reply, reason: p, arrived: time.Now(), Index: 1, Total: 1}
}

// isQuestion reports whether this card asks the operator to choose rather than
// to permit.
func (c *ApprovalCard) isQuestion() bool { return c.Request.IsQuestion() }

// options are the choices, in the order they are shown. Deny is first so that
// the default landing position is the safe one.
func (c *ApprovalCard) options() []string {
	if c.isQuestion() {
		out := make([]string, 0, len(c.Request.Choices)+1)
		out = append(out, c.Request.Choices...)
		return append(out, writeInOption)
	}
	if !c.Request.Grantable {
		return []string{"acknowledge — this rule is never grantable [esc/enter]"}
	}
	return []string{
		"deny — the operation is refused [d/esc]",
		"allow once — record a scoped, reasoned grant [a/enter]",
	}
}

// Toggle ticks or unticks the highlighted row of a multi-select question, and
// reports whether it did anything. Everywhere else the key it is bound to means
// nothing, and saying so lets the caller leave the frame alone.
func (c *ApprovalCard) Toggle() bool {
	if c.stage != stageChoose || !c.isQuestion() || !c.Request.MultiSelect {
		return false
	}
	// The write-in row is not a checkbox: it is a door to the editor.
	if c.option >= len(c.Request.Choices) {
		return false
	}
	if c.picked == nil {
		c.picked = map[int]bool{}
	}
	c.picked[c.option] = !c.picked[c.option]
	return true
}

// NeedsSelection reports that a multi-select question is sitting on the choose
// stage with nothing ticked. The loop uses it to say why Enter did nothing —
// silence there reads as a broken key rather than as an unanswered question.
func (c *ApprovalCard) NeedsSelection() bool {
	if c.stage != stageChoose || !c.isQuestion() || !c.Request.MultiSelect {
		return false
	}
	if c.option >= len(c.Request.Choices) {
		return false
	}
	return len(c.chosen()) == 0
}

// chosen collects the ticked options of a multi-select question.
func (c *ApprovalCard) chosen() []string {
	var out []string
	for i, opt := range c.Request.Choices {
		if c.picked[i] {
			out = append(out, opt)
		}
	}
	return out
}

// Prompt exposes the reason editor when it is active.
func (c *ApprovalCard) Prompt() *Prompt {
	if c.stage == stageReason {
		return c.reason
	}
	return nil
}

// Next moves the highlighted option.
func (c *ApprovalCard) Next(delta int) {
	if c.stage != stageChoose {
		return
	}
	n := len(c.options())
	c.option = (c.option + delta + n) % n
}

// Accept advances the card, returning a decision when it has one.
func (c *ApprovalCard) Accept() (ui.Decision, bool) {
	if !c.Request.Grantable {
		return ui.Decision{
			Allow:  false,
			Reason: "this rule is never grantable by any authority",
		}, true
	}
	switch c.stage {
	case stageChoose:
		if c.isQuestion() {
			return c.answer()
		}
		if c.option == 0 {
			return ui.Decision{Allow: false, Reason: "declined by the operator"}, true
		}
		c.stage = stageReason
		return ui.Decision{}, false
	case stageReason:
		if c.isQuestion() {
			written := trimSpace(c.reason.Value())
			if written == "" {
				// An empty write-in is not an answer. Returning one would put a
				// blank where a human's words belong, and the caller would read
				// it as a question that had been answered.
				return ui.Decision{}, false
			}
			return ui.Decision{
				Allow: true, By: "human",
				Reason: "written in by the operator", WriteIn: written,
			}, true
		}
		reason := c.reason.Value()
		if trimSpace(reason) == "" {
			// Not defaulted to something like "approved by operator". A grant
			// carrying a manufactured reason reads, in a later review, exactly
			// like one carrying a real reason, and there is no way afterwards to
			// tell them apart.
			return ui.Decision{}, false
		}
		return ui.Decision{Allow: true, Reason: reason, By: "human"}, true
	}
	return ui.Decision{}, false
}

// answer resolves a question from the highlighted row.
func (c *ApprovalCard) answer() (ui.Decision, bool) {
	if c.option >= len(c.Request.Choices) {
		c.stage = stageReason
		return ui.Decision{}, false
	}
	if !c.Request.MultiSelect {
		return ui.Decision{
			Allow: true, By: "human",
			Reason: "chosen by the operator",
			Chosen: []string{c.Request.Choices[c.option]},
		}, true
	}
	chosen := c.chosen()
	if len(chosen) == 0 {
		// Enter with nothing ticked does not fall back to the highlighted row.
		// The highlight is where the cursor happens to rest, and turning that
		// into an answer is the same defect this card exists to fix, one level
		// down: a selection nobody made, reported as one they did.
		return ui.Decision{}, false
	}
	return ui.Decision{
		Allow: true, By: "human",
		Reason: "chosen by the operator", Chosen: chosen,
	}, true
}

// Back steps out of the reason editor, or denies from the choice stage.
func (c *ApprovalCard) Back() (ui.Decision, bool) {
	if c.stage == stageReason {
		c.stage = stageChoose
		return ui.Decision{}, false
	}
	if c.isQuestion() {
		// Escape on a question is not an answer and must not read as one. The
		// caller turns this into "nobody answered", which is the honest report.
		return ui.Decision{Allow: false, Reason: "dismissed by the operator without answering"}, true
	}
	return ui.Decision{Allow: false, Reason: "dismissed by the operator"}, true
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n') {
		end--
	}
	return s[start:end]
}

// accent is the card's colour. A question is not a warning: painting it in the
// approval palette teaches an operator that the model asking something is the
// same event as a blocked write, and the two want different reactions.
func (c *ApprovalCard) accent(th Theme) render.Color {
	switch {
	case c.isQuestion():
		return th.Info
	case c.Request.Grantable:
		return th.Warning
	}
	return th.Danger
}

// content builds the card's body, and reports which row the reason editor
// occupies.
//
// Height and Draw both read this rather than each walking the layout on their
// own. They did once, and they disagreed by a single row — which is not a
// cosmetic bug here: the row that fell off the bottom was the allow option, so
// the card presented a decision with one of its two answers invisible.
func (c *ApprovalCard) content(th Theme, width int) (lines []render.Line, reasonRow, optStart int) {
	g := th.Glyphs()
	accent := c.accent(th)
	muted := render.Style{Fg: th.FgMuted, Bg: th.BgOverlay}
	strong := render.Style{Fg: th.Fg, Bg: th.BgOverlay, Attrs: render.Bold}
	body := render.Style{Fg: th.Fg, Bg: th.BgOverlay}

	reasonRow = -1
	add := func(l render.Line) { lines = append(lines, l) }
	blank := func() { lines = append(lines, nil) }

	// A question carries no rule and no blocked subject — it is the model
	// asking, not the gate refusing — so the header those rows describe is not
	// drawn over it. Labelling an answer with a rule and a severity would file
	// it, in the operator's eye, as a grant they issued.
	if !c.isQuestion() {
		add(render.Styled(padLabel("rule"), muted).Append(c.Request.Rule, strong).
			Append("   severity ", muted).
			Append(c.Request.Severity, render.Style{Fg: accent, Bg: th.BgOverlay, Attrs: render.Bold}))
		// The label follows the subject: a blocked shell command shown under the
		// word "path" is a question about a different act than the one being asked.
		add(render.Styled(padLabel(c.Request.SubjectLabel()), muted).Append(c.Request.Path, strong))
	}
	if c.Request.TaskID != "" {
		add(render.Styled(padLabel("task"), muted).Append(c.Request.TaskID, strong))
	}
	if len(lines) > 0 {
		blank()
	}
	for _, l := range render.WrapText(c.Request.Reason, width, body) {
		add(l)
	}
	blank()

	// Syntax-colored diff preview for file writes and edits.
	if c.Request.Diff != "" {
		add(render.Styled("┌─ diff preview ", render.Style{Fg: th.Info, Bg: th.BgOverlay, Attrs: render.Bold}))
		diffLines := strings.Split(c.Request.Diff, "\n")
		maxDiff := 8
		for idx, dl := range diffLines {
			if idx >= maxDiff {
				add(render.Styled(fmt.Sprintf("│ … %d more diff lines", len(diffLines)-maxDiff), muted))
				break
			}
			st := muted
			switch {
			case strings.HasPrefix(dl, "+") && len(dl) > 1 && dl[1] != '+':
				st = render.Style{Fg: th.Success, Bg: th.BgOverlay}
			case strings.HasPrefix(dl, "-") && len(dl) > 1 && dl[1] != '-':
				st = render.Style{Fg: th.Danger, Bg: th.BgOverlay}
			case strings.HasPrefix(dl, "@@"):
				st = render.Style{Fg: th.Info, Bg: th.BgOverlay, Attrs: render.Bold}
			}
			add(render.Styled("│ ", muted).Append(render.TruncateWidth(dl, width-3, "…"), st))
		}
		add(render.Styled("└─", muted))
		blank()
	}

	// Where the clickable choices begin. Draw records these rows so a pointer
	// click is mapped against the layout the operator can see.
	optStart = len(lines)
	for i, opt := range c.options() {
		st := muted
		marker := "  "
		if i == c.option && c.stage == stageChoose {
			st = render.Style{Fg: th.Fg, Bg: th.Selection, Attrs: render.Bold}
			marker = g.Arrow + " "
		}
		// A ticked box is the only thing that distinguishes a multi-select row
		// the operator chose from the one the cursor is merely resting on.
		box := ""
		if c.Request.MultiSelect && i < len(c.Request.Choices) {
			box = "[ ] "
			if c.picked[i] {
				box = "[" + g.Pass + "] "
			}
		}
		row := render.Styled(marker, st).Append(itoa(i+1)+". "+box+opt, st)
		if i == c.option && c.stage == stageChoose {
			row = row.Pad(width, st)
		}
		add(row)
	}

	if c.Request.MultiSelect && c.stage == stageChoose {
		add(render.Styled("space ticks an option; enter sends the ticked ones", muted))
	}

	if c.stage == stageReason {
		blank()
		if c.isQuestion() {
			add(render.Styled("your answer (goes back to the model as you type it)", muted))
		} else {
			add(render.Styled("reason (required — a grant nobody can review later is not issued)", muted))
		}
		reasonRow = len(lines)
		blank()
	}
	if !c.Request.Grantable && !c.isQuestion() {
		add(render.Styled("no grant clears this rule; the operation will be refused either way",
			render.Style{Fg: th.Danger, Bg: th.BgOverlay}))
	}
	return lines, reasonRow, optStart
}

// Height is how many rows the card needs at a width.
func (c *ApprovalCard) Height(th Theme, width int) int {
	inner := width - 6
	if inner < 12 {
		inner = 12
	}
	lines, _, _ := c.content(th, inner)
	// Two rows of frame and one of padding at each end.
	return len(lines) + 4
}

// Draw paints the card and returns the caret position, if the reason editor is
// active.
//
// tick and fxOn drive the border's breathing pulse: a blocked turn is the one
// thing on screen that needs a human, so the frame inhales slowly rather than
// sitting still. When fxOn is false — plain theme, no colour, MANVI_FX=off —
// the border is a flat accent as before.
func (c *ApprovalCard) Draw(b *render.Buffer, r render.Rect, th Theme, tick int, fxOn bool) render.Rect {
	g := th.Glyphs()
	accent := c.accent(th)
	border := accent
	if fxOn {
		// The crest blends a third of the way toward the card's foreground:
		// enough to catch the eye, not enough to strobe it.
		border = accent.Blend(th.Fg, fx.Pulse(tick, 24)*0.4)
	}

	title := " " + g.Warn + " approval required "
	if c.Total > 1 {
		title = " " + g.Warn + " approval " + itoa(c.Index) + " of " + itoa(c.Total) + " "
	}
	waiting := " blocked " + shortDuration(time.Since(c.arrived)) + " "
	if c.isQuestion() {
		title = " ? the model is asking "
		if c.Total > 1 {
			title = " ? question " + itoa(c.Index) + " of " + itoa(c.Total) + " "
		}
		waiting = " waiting " + shortDuration(time.Since(c.arrived)) + " "
	}

	fill := render.Style{Fg: th.Fg, Bg: th.BgOverlay}
	box := render.Box{
		Border: borderFor(th),
		Style:  render.Style{Fg: border, Bg: th.BgOverlay},
		Title: render.Styled(title,
			render.Style{Fg: accent, Bg: th.BgOverlay, Attrs: render.Bold}),
		Subtitle: render.Styled(waiting,
			render.Style{Fg: th.FgSubtle, Bg: th.BgOverlay}),
		Fill: &fill,
	}
	inner := box.Draw(b, r)
	c.cardRect = r
	c.optionRows = c.optionRows[:0]
	c.reasonRect = render.Rect{}
	if inner.Empty() {
		return render.Rect{}
	}
	inner = inner.Pad(1, 2, 1, 2)
	if inner.Empty() {
		return render.Rect{}
	}

	lines, reasonRow, optStart := c.content(th, inner.W)
	nOpts := len(c.options())
	var caret render.Rect
	for i, l := range lines {
		y := inner.Y + i
		if y >= inner.Bottom() {
			// The card was given less room than it asked for. Say so rather
			// than letting an option fall off the bottom unannounced.
			render.Styled("… card truncated; widen or lengthen the terminal to answer",
				render.Style{Fg: th.Danger, Bg: th.BgOverlay, Attrs: render.Bold}).
				DrawIn(b, render.Rect{X: inner.X, Y: inner.Bottom() - 1, W: inner.W, H: 1})
			break
		}
		if i >= optStart && i < optStart+nOpts {
			c.optionRows = append(c.optionRows, render.Rect{X: inner.X, Y: y, W: inner.W, H: 1})
		}
		if i == reasonRow {
			field := render.Rect{X: inner.X, Y: y, W: inner.W, H: 1}
			c.reasonRect = field
			b.Fill(field, ' ', render.Style{Fg: th.Fg, Bg: th.BgInset})
			caret = c.reason.Draw(b, field, th, true)
			continue
		}
		l.Truncate(inner.W).Draw(b, inner.X, y)
	}
	return caret
}

// Click routes a pointer press on the card, reporting (accept, handled).
//
// The grammar is the overlay's, which is also the keys': a click on a row
// moves the highlight there, and a click on the highlighted row confirms it.
// Two steps rather than one is deliberate — this card is the
// human-in-the-loop control, and a permission answered by a stray trackpad
// brush is indistinguishable in the ledger from one answered on purpose.
//
// handled is true for every point inside the card, controls or not: the card
// is modal, and a click that falls through it would select transcript text
// behind the decision being asked.
func (c *ApprovalCard) Click(x, y int) (accept, handled bool) {
	if c.cardRect.Empty() || !c.cardRect.Contains(x, y) {
		return false, false
	}
	if c.stage == stageReason && c.reasonRect.Contains(x, y) {
		// The editor is live; put its caret where the click landed, the same
		// courtesy the composer extends.
		c.reason.SetCursor(c.reason.IndexAt(c.reasonRect.W, y-c.reasonRect.Y, x-c.reasonRect.X))
		return false, true
	}
	for i, row := range c.optionRows {
		if !row.Contains(x, y) {
			continue
		}
		switch {
		case c.stage == stageReason && i == c.option:
			// From the reason stage, the chosen row is the card's own
			// confirm button — otherwise an allow-with-reason could only
			// ever be completed from the keyboard.
			return true, true
		case c.stage != stageChoose || c.option != i:
			// First click: move the highlight. On another row from the
			// reason stage this also steps back to the choices, as Esc does
			// on the keyboard.
			c.stage = stageChoose
			c.option = i
			return false, true
		default:
			// Second click on the highlighted row: what Enter does.
			if c.isQuestion() && c.Request.MultiSelect && i < len(c.Request.Choices) {
				c.Toggle()
				return false, true
			}
			return true, true
		}
	}
	return false, true
}

func borderFor(th Theme) render.Border {
	if th.Unicode {
		return render.Rounded
	}
	return render.ASCII
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return "just now"
	case d < time.Minute:
		return itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return itoa(int(d.Minutes())) + "m"
	}
	return itoa(int(d.Hours())) + "h"
}

// Approver bridges the harness's approval seam into the UI loop.
//
// It implements ui.Approver, so the agent loop asks for permission exactly as
// it does headlessly. The request is handed to the loop as an Action and this
// call blocks on the reply, which is what keeps the decision on the operator's
// side of the seam rather than in a callback that renders its own prompt.
type Approver struct {
	SessionID string
	// Actions is the loop's inbox.
	Actions chan<- Action
}

// Approve puts the request to the operator and waits.
func (a *Approver) Approve(ctx context.Context, req ui.Request) (ui.Decision, error) {
	// Buffered so the loop can answer and move on without waiting for this
	// goroutine to be scheduled.
	reply := make(chan ui.Decision, 1)
	select {
	case a.Actions <- ActionApprovalRequest{SessionID: a.SessionID, Request: req, Reply: reply}:
	case <-ctx.Done():
		return ui.Decision{Allow: false, Reason: "cancelled before it could be put to an operator"}, nil
	}

	select {
	case d := <-reply:
		return d, nil
	case <-ctx.Done():
		// A question that could not be answered is a refusal, never an allow.
		// Treating an unanswerable prompt as consent is the one failure mode an
		// approval seam must not have.
		return ui.Decision{Allow: false, Reason: "cancelled while awaiting an operator"}, nil
	}
}
