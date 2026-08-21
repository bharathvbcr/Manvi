package tui

import (
	"strings"
	"testing"

	"manvi/ui"
	"manvi/ui/render"
)

func questionRequest(multi bool) ui.Request {
	return ui.Request{
		ID: "QUESTION-0001", Rule: "pair.questions.enabled", Severity: "ask",
		Subject: "question", Reason: "Which database backend should we configure?",
		Grantable:   true,
		Choices:     []string{"SQLite WAL mode", "PostgreSQL 16", "MySQL 8"},
		MultiSelect: multi,
	}
}

func raiseQuestion(a *App, multi bool) chan ui.Decision {
	reply := make(chan ui.Decision, 1)
	a.Dispatch(ActionApprovalRequest{SessionID: "S1", Request: questionRequest(multi), Reply: reply})
	return reply
}

// cardText renders a card and returns its visible text, so the assertions below
// check what an operator would actually see rather than what the model holds.
func cardText(c *ApprovalCard) string {
	th := Dark()
	h := c.Height(th, 80)
	b := render.NewBuffer(80, h)
	c.Draw(b, render.Rect{X: 0, Y: 0, W: 80, H: h}, th, 0, false)
	var sb strings.Builder
	for y := 0; y < h; y++ {
		for x := 0; x < 80; x++ {
			if r := b.Cell(x, y).R; r != 0 {
				sb.WriteRune(r)
			}
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestAQuestionCardShowsTheModelsOwnOptions is the fix for the defect: the
// options the model offered reach the screen, instead of the harness picking
// one behind the operator's back.
func TestAQuestionCardShowsTheModelsOwnOptions(t *testing.T) {
	c := NewApprovalCard(questionRequest(false), make(chan ui.Decision, 1))
	text := cardText(c)

	for _, want := range []string{
		"Which database backend should we configure?",
		"SQLite WAL mode", "PostgreSQL 16", "MySQL 8",
		writeInOption,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the card does not show %q:\n%s", want, text)
		}
	}
	// A question is not a grant, and must not be dressed as one.
	if strings.Contains(text, "allow once") || strings.Contains(text, "approval required") {
		t.Errorf("a question was rendered as an approval:\n%s", text)
	}
}

func TestASingleSelectQuestionAnswersWithTheChosenOption(t *testing.T) {
	c := NewApprovalCard(questionRequest(false), make(chan ui.Decision, 1))
	c.Next(1) // move off the first option, so a pass cannot come from the default
	d, done := c.Accept()
	if !done {
		t.Fatal("Enter on a single-select question did not answer it")
	}
	if !d.Answered() || len(d.Chosen) != 1 || d.Chosen[0] != "PostgreSQL 16" {
		t.Fatalf("decision = %#v", d)
	}
	if d.By != "human" {
		t.Errorf("By = %q, want \"human\"", d.By)
	}
}

// TestAMultiSelectQuestionWillNotAnswerFromTheCursor is the same defect one
// level down: the highlight is where the cursor happens to rest, not a choice
// anybody made.
func TestAMultiSelectQuestionWillNotAnswerFromTheCursor(t *testing.T) {
	c := NewApprovalCard(questionRequest(true), make(chan ui.Decision, 1))
	if !c.NeedsSelection() {
		t.Error("NeedsSelection did not report an unticked multi-select")
	}
	if _, done := c.Accept(); done {
		t.Fatal("a multi-select question answered itself from the cursor position")
	}
}

func TestAMultiSelectQuestionAnswersWithTheTickedOptions(t *testing.T) {
	c := NewApprovalCard(questionRequest(true), make(chan ui.Decision, 1))
	if !c.Toggle() {
		t.Fatal("space did not tick the first option")
	}
	c.Next(2)
	if !c.Toggle() {
		t.Fatal("space did not tick the third option")
	}
	if !strings.Contains(cardText(c), "[") {
		t.Errorf("ticked options are not shown as ticked:\n%s", cardText(c))
	}
	d, done := c.Accept()
	if !done {
		t.Fatal("Enter did not send the ticked options")
	}
	if len(d.Chosen) != 2 || d.Chosen[0] != "SQLite WAL mode" || d.Chosen[1] != "MySQL 8" {
		t.Fatalf("decision = %#v", d)
	}
}

func TestTheWriteInRowTakesAnAnswerNoneOfTheOptionsCarry(t *testing.T) {
	c := NewApprovalCard(questionRequest(false), make(chan ui.Decision, 1))
	c.Next(len(c.Request.Choices)) // land on the write-in row
	if _, done := c.Accept(); done {
		t.Fatal("the write-in row answered without an answer being typed")
	}
	p := c.Prompt()
	if p == nil {
		t.Fatal("the write-in row did not open an editor")
	}
	// An empty write-in is not an answer.
	if _, done := c.Accept(); done {
		t.Fatal("an empty write-in was accepted as an answer")
	}
	for _, r := range "use DuckDB" {
		p.Insert(r)
	}
	d, done := c.Accept()
	if !done || !d.Answered() || d.WriteIn != "use DuckDB" {
		t.Fatalf("done=%v decision=%#v", done, d)
	}
}

// TestEscapeOnAQuestionIsNotAnAnswer: dismissing is the operator declining, and
// the caller must be able to see that no answer came back.
func TestEscapeOnAQuestionIsNotAnAnswer(t *testing.T) {
	c := NewApprovalCard(questionRequest(false), make(chan ui.Decision, 1))
	d, done := c.Back()
	if !done {
		t.Fatal("Escape did not resolve the card")
	}
	if d.Answered() || d.Allow {
		t.Fatalf("a dismissed question came back answered: %#v", d)
	}
}

// TestSpaceTicksAnOptionThroughTheLoop checks the binding, not just the card:
// space is a printable rune, and the table names it "space".
func TestSpaceTicksAnOptionThroughTheLoop(t *testing.T) {
	a, _ := newTestApp()
	raiseQuestion(a, true)
	if effects := a.Dispatch(ActionRune{Runes: []rune{' '}}); len(effects) != 0 {
		t.Fatalf("space resolved the card instead of ticking: %#v", effects)
	}
	card := a.Current().Approval()
	if card == nil {
		t.Fatal("the question card is gone")
	}
	if card.NeedsSelection() {
		t.Fatal("space did not tick the highlighted option")
	}
	effects := a.Dispatch(ActionKey{Binding: "enter"})
	if len(effects) != 1 {
		t.Fatalf("Enter after a tick produced %#v", effects)
	}
	d := effects[0].(EffectDecide).Decision
	if len(d.Chosen) != 1 || d.Chosen[0] != "SQLite WAL mode" {
		t.Fatalf("decision = %#v", d)
	}
}

// TestADigitTicksRatherThanAnswersOnAMultiSelect: answering on the first digit
// would send a one-option answer to a question that asked for several.
func TestADigitTicksRatherThanAnswersOnAMultiSelect(t *testing.T) {
	a, _ := newTestApp()
	raiseQuestion(a, true)
	if effects := a.Dispatch(ActionRune{Runes: []rune{'2'}}); len(effects) != 0 {
		t.Fatalf("a digit answered a multi-select question: %#v", effects)
	}
	if a.Current().Approval() == nil {
		t.Fatal("the card was resolved by a digit")
	}
}

// TestADigitAnswersASingleSelectQuestion keeps the fast path an operator
// reaches for under time pressure.
func TestADigitAnswersASingleSelectQuestion(t *testing.T) {
	a, _ := newTestApp()
	raiseQuestion(a, false)
	effects := a.Dispatch(ActionRune{Runes: []rune{'3'}})
	if len(effects) != 1 {
		t.Fatalf("got %#v", effects)
	}
	d := effects[0].(EffectDecide).Decision
	if len(d.Chosen) != 1 || d.Chosen[0] != "MySQL 8" {
		t.Fatalf("decision = %#v", d)
	}
}

// TestAnApprovalIsStillAnApproval guards the seam's original job against the
// question support added beside it.
func TestAnApprovalIsStillAnApproval(t *testing.T) {
	c := NewApprovalCard(blockedRequest(true), make(chan ui.Decision, 1))
	if c.isQuestion() {
		t.Fatal("a blocked write was treated as a question")
	}
	text := cardText(c)
	if !strings.Contains(text, "deny") || !strings.Contains(text, "allow once") {
		t.Errorf("the approval card lost its options:\n%s", text)
	}
	if c.Toggle() {
		t.Error("an approval card ticked something")
	}
}
