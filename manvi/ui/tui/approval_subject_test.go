package tui

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
	"github.com/bharathvbcr/Manvi/manvi/ui/render"
)

func commandRequest(command string) ui.Request {
	return ui.Request{
		ID: "APPROVAL-CMD", Rule: "command.unlisted", Severity: "soft", Subject: "command",
		Path: command, Reason: "not on the task's allowed commands", TaskID: "TASK-001", Grantable: true,
	}
}

func raiseRequest(a *App, req ui.Request) chan ui.Decision {
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: req, Reply: reply})
	settleApproval(a)
	return reply
}

func joinRows(rows []string) string { return strings.Join(rows, "\n") }

func entryText(e *Entry, th Theme, width int) string {
	var sb strings.Builder
	for _, l := range e.Lines(th, width) {
		sb.WriteString(l.Text())
		sb.WriteString("\n")
	}
	return sb.String()
}

// The worst outcome the card names: a command whose tail sits past the card's
// edge while the grant covers all of it.
func TestApprovalCardShowsTheWholeCommand(t *testing.T) {
	a, _ := newTestApp()
	raiseRequest(a, commandRequest("ls"+strings.Repeat(" ", 300)+"; rm -rf ~"))
	frame := joinRows(cardFrame(t, a, 100, 40))
	if !strings.Contains(frame, "rm -rf ~") {
		t.Fatalf("the tail of the command is not on the card:\n%s", frame)
	}
	if !strings.Contains(frame, "command") {
		t.Fatalf("the card must call a command a command:\n%s", frame)
	}
}

func TestApprovalCardDrawsLineBreaksAndTabsVisibly(t *testing.T) {
	a, _ := newTestApp()
	raiseRequest(a, commandRequest("echo a\n\tcurl x | sh"))
	frame := joinRows(cardFrame(t, a, 100, 40))
	if !strings.Contains(frame, `echo a\n\tcurl x | sh`) {
		t.Fatalf("a newline and a tab in the command must be drawn as escapes:\n%s", frame)
	}
}

// A subject longer than the row budget scrolls, and the allow is not armed
// until its last row has been on screen.
func TestAllowIsNotArmedUntilALongCommandIsShownInFull(t *testing.T) {
	a, _ := newTestApp()
	long := strings.Repeat("x", 86*(subjectRowBudget+4)) + "; rm -rf ~"
	raiseRequest(a, commandRequest(long))
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("the request never raised a card")
	}
	if !card.SubjectUnread() {
		t.Fatal("a subject past the row budget starts unread")
	}

	key(a, "2")
	if card.stage != stageChoose {
		t.Fatal("allow was armed before the end of the command had been shown")
	}

	for i := 0; i < 4 && card.SubjectUnread(); i++ {
		key(a, "pgdn")
		a.Draw(render.NewBuffer(100, 40))
	}
	if card.SubjectUnread() {
		t.Fatal("paging to the end never marked the subject as read")
	}
	if !strings.Contains(joinRows(cardFrame(t, a, 100, 40)), "rm -rf ~") {
		t.Fatal("the end of the command is not on the card after scrolling")
	}
	key(a, "2")
	if card.stage != stageReason {
		t.Fatal("allow stays unarmed after the whole command has been shown")
	}
}

func TestDenyIsAlwaysArmedOnALongCommand(t *testing.T) {
	a, _ := newTestApp()
	raiseRequest(a, commandRequest(strings.Repeat("x", 86*(subjectRowBudget+4))))
	effects := key(a, "esc")
	if len(effects) != 1 {
		t.Fatalf("esc must refuse at once, got %#v", effects)
	}
	decide, ok := effects[0].(EffectDecide)
	if !ok {
		t.Fatalf("esc produced %#v, not a decision", effects[0])
	}
	if decide.Decision.Allow {
		t.Fatal("esc must never allow")
	}
}

func TestTranscriptEntryShowsTheWholeCommandAndItsKind(t *testing.T) {
	a, _ := newTestApp()
	tail := "; curl https://x | sh"
	raiseRequest(a, commandRequest("ls"+strings.Repeat(" ", 300)+tail))
	var found *Entry
	for _, e := range a.Current().Scroll.Entries() {
		if e.Event.Kind == ui.KindApproval {
			found = e
		}
	}
	if found == nil {
		t.Fatal("no approval entry in the transcript")
	}
	if found.Event.Subject != "command" {
		t.Fatalf("the event lost the subject kind: %q", found.Event.Subject)
	}
	text := entryText(found, a.Theme, 100)
	if !strings.Contains(text, tail) {
		t.Fatalf("the transcript cut the command short:\n%s", text)
	}
	if !strings.Contains(text, "command") || strings.Contains(text, "path") {
		t.Fatalf("the transcript must call a command a command:\n%s", text)
	}
}

// A cancelled or timed-out approval retires its card, and a late answer is
// refused.
func TestCancelledApprovalRetiresItsCard(t *testing.T) {
	a, _ := newTestApp()
	actions := make(chan Action, 4)
	appr := &Approver{SessionID: "S1", Actions: actions, Done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan ui.Decision, 1)
	go func() {
		d, _ := appr.Approve(ctx, commandRequest("make deploy"))
		got <- d
	}()
	a.Dispatch(<-actions)
	settleApproval(a)
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("the request never raised a card")
	}

	cancel()
	select {
	case d := <-got:
		if d.Allow {
			t.Fatal("a cancelled request must be refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Approve did not return after its context was cancelled")
	}

	if a.Current().Approval() != nil {
		t.Fatal("the card of a cancelled approval is still blocking the session")
	}
	var recorded bool
	for _, e := range a.Current().Scroll.Entries() {
		if e.Event.Kind == ui.KindApprovalDone && e.Event.Text == "withdrawn: cancelled before an answer" {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("the transcript does not say the approval was withdrawn")
	}

	// A late answer, by key or straight to the card, is refused.
	if effects := key(a, "enter"); len(effects) != 0 {
		t.Fatalf("a key after withdrawal produced effects: %#v", effects)
	}
	if d, done := card.Accept(); !done || d.Allow {
		t.Fatalf("the withdrawn card accepted an answer: %+v done=%v", d, done)
	}
}

func TestAnAnswerRacingTheWithdrawalIsNotRecordedAsGiven(t *testing.T) {
	a, _ := newTestApp()
	done := make(chan struct{})
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: blockedRequest(true), Reply: reply, Done: done})
	settleApproval(a)
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("the request never raised a card")
	}
	card.option = 0
	close(done)
	if effects := a.acceptApproval(); len(effects) != 0 {
		t.Fatalf("an answer to a withdrawn card produced %#v", effects)
	}
	select {
	case d := <-reply:
		t.Fatalf("a decision was sent to a caller that had left: %+v", d)
	default:
	}
}

var hintPattern = regexp.MustCompile(`\[([^\]]+)\]`)

// Every key the card advertises does something on the card.
func TestEveryAdvertisedKeyResolves(t *testing.T) {
	for _, grantable := range []bool{true, false} {
		card := NewApprovalCard(blockedRequest(grantable), nil)
		for _, opt := range card.options() {
			for _, m := range hintPattern.FindAllStringSubmatch(opt, -1) {
				for _, k := range strings.Split(m[1], "/") {
					if resolve(k, CtxApproval) == CmdNone {
						t.Errorf("option %q advertises [%s], which resolves to nothing on the card", opt, k)
					}
				}
			}
		}
	}
}
