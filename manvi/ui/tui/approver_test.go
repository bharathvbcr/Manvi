package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bharathvbcr/Manvi/manvi/ui"
)

// These are the approval-seam guarantees the line-mode Prompter used to carry,
// held against the Approver the terminal face actually runs.

// TestAnUnanswerableApprovalIsARefusal is the one failure mode an approval seam
// must not have: treating a question nobody answered as consent.
func TestAnUnanswerableApprovalIsARefusal(t *testing.T) {
	t.Run("nobody takes the request", func(t *testing.T) {
		// Unbuffered and never read: the loop is not there to raise the card.
		ap := &Approver{SessionID: "S1", Actions: make(chan Action)}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		d, err := ap.Approve(ctx, blockedRequest(true))
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow {
			t.Fatal("a request no loop could raise was read as approval")
		}
	})

	t.Run("cancelled before it is raised", func(t *testing.T) {
		ap := &Approver{SessionID: "S1", Actions: make(chan Action)}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		d, err := ap.Approve(ctx, blockedRequest(true))
		if err != nil {
			t.Fatal(err)
		}
		if d.Allow {
			t.Fatal("a cancelled request was read as approval")
		}
	})
}

// TestAnUnanswerableQuestionIsUnanswered: a question that reached nobody is
// not an answer, for the same reason an unanswerable approval is not an allow.
func TestAnUnanswerableQuestionIsUnanswered(t *testing.T) {
	ap := &Approver{SessionID: "S1", Actions: make(chan Action)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := ap.Approve(ctx, questionRequest(false))
	if err != nil {
		t.Fatal(err)
	}
	if d.Answered() || d.Allow {
		t.Fatalf("a question nobody could answer came back answered: %#v", d)
	}
}

// TestAQuestionWithNoSessionAnswersNothing is the unattended default for a
// question: nobody can see it, so nothing is answered.
func TestAQuestionWithNoSessionAnswersNothing(t *testing.T) {
	a, _ := newTestApp()
	effects := a.Dispatch(ActionApprovalRequest{
		SessionID: "nonexistent", Request: questionRequest(false), Reply: make(chan ui.Decision, 1),
	})
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	if d := effects[0].(EffectDecide).Decision; d.Answered() || d.Allow {
		t.Fatalf("a question with no view was answered: %#v", d)
	}
}

// TestADigitThatIsNotAnOptionDoesNotAnswer: an option that was never offered is
// not an answer.
func TestADigitThatIsNotAnOptionDoesNotAnswer(t *testing.T) {
	a, _ := newTestApp()
	raiseQuestion(a, false)
	if effects := a.Dispatch(ActionRune{Runes: []rune{'9'}}); len(effects) != 0 {
		t.Fatalf("a digit past the offered options decided: %#v", effects)
	}
	v := a.Current()
	if v == nil {
		t.Fatal("no session is current")
	}
	if v.Approval() == nil {
		t.Fatal("the question was dismissed by a digit that names no option")
	}
}

// TestASingleSelectQuestionCannotTickSeveral: picking one of two answers a
// person gave is the harness deciding, not the person, so a single-select
// question offers no ticks at all.
func TestASingleSelectQuestionCannotTickSeveral(t *testing.T) {
	c := NewApprovalCard(questionRequest(false), make(chan ui.Decision, 1))
	if c.Toggle() {
		t.Fatal("a single-select question accepted a tick")
	}
}

// TestTheApprovalAndItsAnswerBothReachTheTranscript: an answer with no
// recorded question cannot be audited.
func TestTheApprovalAndItsAnswerBothReachTheTranscript(t *testing.T) {
	a, _ := newTestApp()
	raiseApproval(a, true)
	key(a, "down") // allow once
	key(a, "enter")
	typeText(a, "needed for the fix")
	if effects := key(a, "enter"); len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}

	v := a.Current()
	if v == nil {
		t.Fatal("no session is current")
	}
	var asked, decided *Entry
	for _, e := range v.Scroll.Entries() {
		switch e.Event.Kind {
		case ui.KindApproval:
			asked = e
		case ui.KindApprovalDone:
			decided = e
		}
	}
	if asked == nil || decided == nil {
		t.Fatal("the transcript does not carry both the request and the decision")
	}
	if asked.Event.ApprovalID == "" || asked.Event.ApprovalID != decided.Event.ApprovalID {
		t.Fatalf("the question and answer are not linked: %q vs %q",
			asked.Event.ApprovalID, decided.Event.ApprovalID)
	}
	if !strings.Contains(decided.Event.Text, "needed for the fix") {
		t.Fatalf("the recorded reason was lost: %q", decided.Event.Text)
	}
}
