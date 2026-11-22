package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

// advB26Frame paints the whole app at w x h and returns every row as text.
func advB26Frame(a *App, w, h int) []string {
	b := render.NewBuffer(w, h)
	a.Draw(b)
	rows := make([]string, h)
	for y := 0; y < h; y++ {
		var sb strings.Builder
		for x := 0; x < w; x++ {
			if c := b.Cell(x, y); !c.IsContinuation() {
				sb.WriteRune(c.R)
			}
		}
		rows[y] = sb.String()
	}
	return rows
}

func advB26Long(tail string) string {
	return strings.Repeat("x", 86*(subjectRowBudget+4)) + tail
}

// Resize: the card was drawn wide enough that the whole command fit, so it
// latched "seen". Narrowing the terminal pushes the tail off the visible
// window; the allow must not stay armed on a tail that is not on screen now.
func TestAdvB26SubjectResizeAfterSeenDoesNotKeepAllowArmed(t *testing.T) {
	a, _ := newTestApp()
	a.Dispatch(ActionResize{W: 400, H: 40})
	raiseRequest(a, commandRequest(strings.Repeat("x", 800)+"; rm -rf ZZTAILZZ"))
	wide := strings.Join(advB26Frame(a, 400, 40), "\n")
	if !strings.Contains(wide, "ZZTAILZZ") {
		t.Skip("the command did not fit on the wide card; the premise does not hold")
	}
	a.Dispatch(ActionResize{W: 40, H: 30})
	narrow := strings.Join(advB26Frame(a, 40, 30), "\n")
	card := a.Current().Approval()
	if strings.Contains(narrow, "ZZTAILZZ") {
		t.Skip("tail still visible after shrinking")
	}
	key(a, "2")
	if card.stage != stageChoose {
		t.Fatalf("allow armed (stage=%v) although the tail of the command is not on screen after the card narrowed:\n%s", card.stage, narrow)
	}
}

// Same shape, scrolling: the operator pages to the end, then the terminal
// narrows. Seen-ness must be a property of what is on screen now.
func TestAdvB26SubjectResizeAfterScrollToEnd(t *testing.T) {
	a, _ := newTestApp()
	raiseRequest(a, commandRequest(advB26Long("; rm -rf ZZTAILZZ")))
	for i := 0; i < 6; i++ {
		key(a, "pgdn")
		advB26Frame(a, 100, 40)
	}
	card := a.Current().Approval()
	if card.SubjectUnread() {
		t.Fatal("premise: paging to the end should clear unread")
	}
	a.Dispatch(ActionResize{W: 30, H: 40})
	frame := strings.Join(advB26Frame(a, 30, 40), "\n")
	if strings.Contains(frame, "ZZTAILZZ") {
		t.Skip("tail still visible")
	}
	key(a, "2")
	if card.stage != stageChoose {
		t.Fatalf("allow armed with the tail off screen after a resize:\n%s", frame)
	}
}

// Armed implies the tail is in the frame that was actually painted, whatever
// the terminal height.
func TestAdvB26SubjectShortTerminalNeverArmsWithoutTail(t *testing.T) {
	for _, h := range []int{6, 9, 12, 16} {
		a, _ := newTestApp()
		a.Dispatch(ActionResize{W: 100, H: h})
		raiseRequest(a, commandRequest(advB26Long("; rm -rf ZZTAILZZ")))
		card := a.Current().Approval()
		for i := 0; i < 30; i++ {
			key(a, "pgdn")
			frame := strings.Join(advB26Frame(a, 100, h), "\n")
			if !card.SubjectUnread() && !strings.Contains(frame, "ZZTAILZZ") {
				t.Fatalf("height %d: subject marked read after paging, but the tail was never in a painted %d-row frame:\n%s", h, h, frame)
			}
		}
	}
}

// Two long approvals back to back: the second must start unread, and must not
// be armed by the first one's reading.
func TestAdvB26SubjectSecondQueuedCardDoesNotInheritSeen(t *testing.T) {
	a, _ := newTestApp()
	raiseRequest(a, commandRequest(advB26Long("; first")))
	r2 := commandRequest(advB26Long("; rm -rf ZZTAILZZ"))
	r2.ID = "APPROVAL-2"
	raiseRequest(a, r2)
	for i := 0; i < 6; i++ {
		key(a, "pgdn")
		advB26Frame(a, 100, 40)
	}
	key(a, "esc") // deny the first
	card := a.Current().Approval()
	if card == nil || card.Request.ID != "APPROVAL-2" {
		t.Fatalf("second card not promoted: %+v", card)
	}
	key(a, "2")
	if card.stage != stageChoose {
		t.Fatal("second card allowed before it was read (inherited the first card's seen state)")
	}
	advB26Frame(a, 100, 40)
	a.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	key(a, "2")
	if card.stage != stageChoose {
		t.Fatal("second card allowed after one paint of its top rows only")
	}
}

// Never drawn at all (no Draw between raise and key) with the quiet gate
// satisfied by the clock: the card must not arm.
func TestAdvB26SubjectNeverPaintedCardDoesNotArm(t *testing.T) {
	card := NewApprovalCard(commandRequest(advB26Long("; rm -rf ZZTAILZZ")), nil)
	card.option = 1
	card.Accept()
	if card.stage != stageChoose {
		t.Fatal("a card whose subject was never laid out advanced to the reason stage: SubjectUnread is false until content() has run")
	}
}

// A hostile subject, painted on the card: nothing a terminal would act on or
// reorder may reach a cell, and the hidden-looking characters must be marked.
func TestAdvB26SubjectHostileContentOnCard(t *testing.T) {
	cases := map[string]string{
		"esc":  "ls \x1b[2K\x1b[1Arm -rf ~",
		"bidi": "echo \u202e hsiloof",
		"cr":   "echo safe\rrm -rf ~",
		"zw":   "echo a" + strings.Repeat("\u200b", 40) + "; rm -rf ~",
		"c1":   "echo \u009b2J; rm -rf ~",
	}
	for name, subj := range cases {
		a, _ := newTestApp()
		raiseRequest(a, commandRequest(subj))
		frame := strings.Join(advB26Frame(a, 100, 40), "\n")
		for _, bad := range []string{"\x1b", "\u202e", "\u009b", "\r"} {
			if strings.Contains(frame, bad) {
				t.Errorf("%s: raw %q reached the painted card", name, bad)
			}
		}
		if !strings.Contains(frame, "rm -rf ~") && name != "bidi" {
			t.Errorf("%s: the dangerous tail is not visible:\n%s", name, frame)
		}
		switch name {
		case "esc", "bidi", "c1":
			// The line renderer draws these as markers; the card must show the
			// same text rather than dropping the character without trace.
			if want := ui.Sanitize(subj); !strings.Contains(frame, want) {
				t.Errorf("%s: the card does not show %q (what the line renderer shows); the control character is dropped silently:\n%s", name, want, frame)
			}
		}
	}
}

// A megabyte of subject: the card and the transcript stay bounded in what they
// build.
func TestAdvB26SubjectMegabyteIsBounded(t *testing.T) {
	a, _ := newTestApp()
	huge := strings.Repeat("a", 1<<20) + "; rm -rf ZZTAILZZ"
	start := time.Now()
	raiseRequest(a, commandRequest(huge))
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("raising a 1 MB subject took %v", d)
	}
	card := a.Current().Approval()
	lines, _, _ := card.content(a.Theme, 100)
	if len(lines) > 200 {
		t.Fatalf("the card builds %d rows for a 1 MB subject; unbounded", len(lines))
	}
	var worst int
	for _, e := range a.Current().Scroll.Entries() {
		if n := len(e.Lines(a.Theme, 100)); n > worst {
			worst = n
		}
	}
	if worst > 200 {
		t.Fatalf("a transcript entry holds %d rows for a 1 MB subject; unbounded", worst)
	}
}

// Withdrawal: the card is retired on first access, so the keystroke that was
// meant for the card is routed to the composer. Enter there submits the draft.
func TestAdvB26SubjectEnterAfterWithdrawalDoesNotSubmitDraft(t *testing.T) {
	a, _ := newTestApp()
	typeText(a, "delete every branch")
	done := make(chan struct{})
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: reply, Done: done})
	settleApproval(a)
	if a.Current().Approval() == nil {
		t.Fatal("no card")
	}
	close(done)
	for _, e := range key(a, "enter") {
		if s, ok := e.(EffectSubmit); ok {
			t.Fatalf("Enter aimed at a card that was withdrawn the instant before submitted the operator's draft: %q", s.Text)
		}
	}
}

// Same for a digit: it lands in the composer rather than being swallowed.
func TestAdvB26SubjectDigitAfterWithdrawalIsNotTyped(t *testing.T) {
	a, _ := newTestApp()
	done := make(chan struct{})
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: make(chan ui.Decision, 1), Done: done})
	settleApproval(a)
	close(done)
	a.Dispatch(ActionRune{Runes: []rune{'2'}})
	if got := mustCurrent(a).Prompt.Value(); got != "" {
		t.Fatalf("a digit meant for the card was typed into the composer: %q", got)
	}
}

// Retirement while another card is queued: the stray key that finds the first
// card gone must not answer the second.
func TestAdvB26SubjectKeyAfterWithdrawalDoesNotAnswerNextCard(t *testing.T) {
	a, _ := newTestApp()
	done := make(chan struct{})
	r1 := make(chan ui.Decision, 1)
	r2 := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: r1, Done: done})
	second := blockedRequest(true)
	second.ID = "APPROVAL-2"
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: second, Reply: r2})
	settleApproval(a)
	close(done)
	for _, k := range []string{"enter", "1", "2", "enter"} {
		for _, e := range key(a, k) {
			if d, ok := e.(EffectDecide); ok {
				t.Fatalf("key %q right after the first card withdrew produced a decision on %v: %+v", k, d.Reply == r2, d.Decision)
			}
		}
	}
	if c := a.Current().Approval(); c == nil || c.Request.ID != "APPROVAL-2" {
		t.Fatal("the second card did not show after the first was retired")
	}
}

// A stray esc meant for the withdrawn card denies the next, unseen card: deny is
// not gated by the quiet period, so a refusal lands on a card nobody has read.
func TestAdvB26SubjectEscAfterWithdrawalDoesNotDenyNextCard(t *testing.T) {
	a, _ := newTestApp()
	done := make(chan struct{})
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: make(chan ui.Decision, 1), Done: done})
	second := blockedRequest(true)
	second.ID = "APPROVAL-2"
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: second, Reply: make(chan ui.Decision, 1)})
	settleApproval(a)
	close(done)
	for _, e := range key(a, "esc") {
		if d, ok := e.(EffectDecide); ok {
			t.Fatalf("esc aimed at the withdrawn card answered the next one, which was never painted: %+v", d.Decision)
		}
	}
}

// Double cancel and answer after timeout record exactly one withdrawal.
func TestAdvB26SubjectDoubleWithdrawRecordsOnce(t *testing.T) {
	a, _ := newTestApp()
	done := make(chan struct{})
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: make(chan ui.Decision, 1), Done: done})
	settleApproval(a)
	close(done)
	a.Current().Approval()
	a.Current().Approval()
	a.Current().PendingApprovals()
	n := 0
	for _, e := range a.Current().Scroll.Entries() {
		if e.Event.Kind == ui.KindApprovalDone && e.Event.Text == withdrawnText {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("withdrawal recorded %d times", n)
	}
}
